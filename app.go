package main

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// The platform lxs binary + the network's genesis are embedded so the app is a
// single self-contained download. CI copies the right-OS binary to binaries/lxs
// before `wails build`. Extracted to the app's config dir on first run.
//
//go:embed binaries/lxs
var lxsBinary []byte

//go:embed binaries/lxs-genesis.json
var genesisFile []byte

// Baked network config — the user only pastes their address.
const (
	poolURL = "https://lxsnetwork.duckdns.org"
	rpcURL  = "https://lxsnetwork.duckdns.org"
	seed    = "/ip4/79.72.25.166/tcp/30303/p2p/12D3KooWRSSSocSqWG978SWKpimQsti4WkmjytQX7g1qgJKnNzuA"
)

var (
	reHash  = regexp.MustCompile(`hashrate ~?([0-9.]+)\s*H/s`)
	reAddr  = regexp.MustCompile(`address\s+(0x[0-9a-fA-F]{40})`)
	reKey   = regexp.MustCompile(`private key\s+(0x[0-9a-fA-F]{64})`)
	reValid = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)
)

type App struct {
	ctx     context.Context
	mu      sync.Mutex
	cmd     *exec.Cmd
	running bool
	address string
	mode    string
	logbuf  []string
	hashHs  float64
	shares  int
	blocks  int
	balance string
	binPath string
	genPath string
	dataDir string
}

func NewApp() *App { return &App{balance: "—", mode: "pool"} }

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	cfg, _ := os.UserConfigDir()
	a.dataDir = filepath.Join(cfg, "LXS Miner")
	_ = os.MkdirAll(filepath.Join(a.dataDir, "data"), 0o700)

	binName := "lxs"
	if runtime.GOOS == "windows" {
		binName = "lxs.exe"
	}
	a.binPath = filepath.Join(a.dataDir, binName)
	a.genPath = filepath.Join(a.dataDir, "lxs-genesis.json")
	_ = writeIfChanged(a.binPath, lxsBinary, 0o755)
	_ = writeIfChanged(a.genPath, genesisFile, 0o644)

	// Load a previously created/saved address so the field is pre-filled.
	if b, err := os.ReadFile(filepath.Join(a.dataDir, "wallet.json")); err == nil {
		var w struct{ Address string }
		if json.Unmarshal(b, &w) == nil {
			a.address = w.Address
		}
	}
}

// writeIfChanged only rewrites the file when its bytes differ (a new binary can
// share the old one's size, so compare content, not length).
func writeIfChanged(path string, data []byte, mode os.FileMode) error {
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, data) {
		return nil
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

// ---------- bound methods (called from the UI) ----------

// SavedAddress is the address remembered from a previous session (or "").
func (a *App) SavedAddress() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.address
}

// CreateWallet generates a fresh keypair via `lxs keygen`, saves it, and returns
// the address + private key so the UI can show the backup warning.
func (a *App) CreateWallet() (map[string]string, error) {
	out, err := exec.Command(a.binPath, "keygen").Output()
	if err != nil {
		return nil, fmt.Errorf("could not generate a wallet: %w", err)
	}
	addr := reAddr.FindStringSubmatch(string(out))
	key := reKey.FindStringSubmatch(string(out))
	if addr == nil || key == nil {
		return nil, fmt.Errorf("could not read the generated wallet")
	}
	a.mu.Lock()
	a.address = addr[1]
	a.mu.Unlock()
	b, _ := json.Marshal(map[string]string{"address": addr[1], "key": key[1]})
	_ = os.WriteFile(filepath.Join(a.dataDir, "wallet.json"), b, 0o600)
	return map[string]string{"address": addr[1], "key": key[1],
		"path": filepath.Join(a.dataDir, "wallet.json")}, nil
}

// StartMining launches the miner. mode is "pool" or "solo".
func (a *App) StartMining(address, mode string) error {
	address = strings.TrimSpace(address)
	if !reValid.MatchString(address) {
		return fmt.Errorf("Enter a valid LXS address (0x… 40 hex), or press Create wallet.")
	}
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return nil
	}
	a.address, a.mode = address, mode
	a.logbuf, a.shares, a.blocks, a.hashHs = nil, 0, 0, 0
	dataDir := filepath.Join(a.dataDir, "data")
	var args []string
	if mode == "solo" {
		args = []string{"mine", "-coinbase", address, "-genesis", a.genPath,
			"-p2p-port", "30303", "-bootstrap", seed, "-datadir", dataDir}
	} else {
		args = []string{"mine", "-pool", poolURL, "-coinbase", address, "-datadir", dataDir}
	}
	cmd := exec.Command(a.binPath, args...)
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		a.mu.Unlock()
		return fmt.Errorf("could not start the miner: %w", err)
	}
	a.cmd = cmd
	a.running = true
	a.append("Starting miner (" + mode + ")…")
	a.mu.Unlock()

	go a.readOutput(stdout)
	go a.pollBalance()
	return nil
}

func (a *App) StopMining() {
	a.mu.Lock()
	c := a.cmd
	a.mu.Unlock()
	if c != nil && c.Process != nil {
		_ = c.Process.Kill()
	}
}

func (a *App) readOutput(r interface{ Read([]byte) (int, error) }) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		a.mu.Lock()
		a.append(line)
		if strings.Contains(line, "share accepted") {
			a.shares++
		}
		if strings.Contains(line, "POOL WON block") || strings.Contains(line, "mined block") {
			a.blocks++
		}
		if m := reHash.FindStringSubmatch(line); m != nil {
			fmt.Sscanf(m[1], "%f", &a.hashHs)
		}
		a.mu.Unlock()
	}
	a.mu.Lock()
	a.running = false
	a.append("Miner stopped.")
	a.mu.Unlock()
}

func (a *App) append(line string) {
	a.logbuf = append(a.logbuf, line)
	if len(a.logbuf) > 300 {
		a.logbuf = a.logbuf[len(a.logbuf)-300:]
	}
}

func (a *App) pollBalance() {
	for {
		a.mu.Lock()
		running, addr := a.running, a.address
		a.mu.Unlock()
		if addr != "" {
			if v, err := rpcBalance(addr); err == nil {
				a.mu.Lock()
				a.balance = v
				a.mu.Unlock()
			}
		}
		if !running {
			return
		}
		time.Sleep(15 * time.Second)
	}
}

// GetState is polled by the UI ~every 2s for everything it shows.
func (a *App) GetState() map[string]interface{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	hr := "—"
	if a.hashHs > 0 {
		if a.hashHs >= 1e6 {
			hr = fmt.Sprintf("%.2f MH/s", a.hashHs/1e6)
		} else if a.hashHs >= 1e3 {
			hr = fmt.Sprintf("%.1f kH/s", a.hashHs/1e3)
		} else {
			hr = fmt.Sprintf("%.0f H/s", a.hashHs)
		}
	}
	tail := a.logbuf
	if len(tail) > 120 {
		tail = tail[len(tail)-120:]
	}
	return map[string]interface{}{
		"running":  a.running,
		"address":  a.address,
		"mode":     a.mode,
		"balance":  a.balance,
		"hashrate": hr,
		"shares":   a.shares,
		"blocks":   a.blocks,
		"log":      strings.Join(tail, "\n"),
	}
}

func rpcBalance(addr string) (string, error) {
	body, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0", "method": "eth_getBalance", "params": []interface{}{addr, "latest"}, "id": 1})
	resp, err := http.Post(rpcURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var d struct{ Result string }
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return "", err
	}
	if d.Result == "" {
		return "", fmt.Errorf("no result")
	}
	wei, ok := new(big.Int).SetString(strings.TrimPrefix(d.Result, "0x"), 16)
	if !ok {
		return "", fmt.Errorf("bad balance")
	}
	whole := new(big.Int).Div(wei, big.NewInt(1e18))
	frac := new(big.Int).Mod(wei, big.NewInt(1e18))
	f := fmt.Sprintf("%018d", frac)[:4]
	return fmt.Sprintf("%s.%s", groupThousands(whole.String()), f), nil
}

func groupThousands(s string) string {
	n := len(s)
	if n <= 3 {
		return s
	}
	var out []string
	for n > 3 {
		out = append([]string{s[n-3 : n]}, out...)
		n -= 3
	}
	out = append([]string{s[:n]}, out...)
	return strings.Join(out, ",")
}
