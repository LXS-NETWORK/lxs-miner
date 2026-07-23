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
	poolURL     = "https://lxsnetwork.duckdns.org"
	rpcURL      = "https://lxsnetwork.duckdns.org"
	seed        = "/ip4/79.72.25.166/tcp/30303/p2p/12D3KooWRSSSocSqWG978SWKpimQsti4WkmjytQX7g1qgJKnNzuA"
	totalMined  = 100000000.0 // LXS mined over ~500 years
	totalBlocks = 66000000    // approx blocks until the reward rounds to zero
	halving     = 1000000
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
	status  string // "idle" | "mining" | "paused"
	address string
	mode    string
	logbuf  []string
	hashHs  float64
	shares  int
	blocks  int
	balance string

	uptimeBase float64 // accumulated seconds before the current running segment
	segStart   time.Time

	// network (refreshed by a background poller)
	netHeight  int64
	coinsMined float64
	minersNow  int
	poolHashHs float64
	difficulty string
	diffRaw    int64   // raw difficulty — expected hashes per block
	blockTime  float64 // seconds
	poolPaid   string
	poolBlocks int

	autoStart bool

	binPath string
	genPath string
	dataDir string
}

func NewApp() *App {
	return &App{balance: "—", mode: "pool", status: "idle", difficulty: "—", poolPaid: "0"}
}

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

	if b, err := os.ReadFile(filepath.Join(a.dataDir, "wallet.json")); err == nil {
		var w struct{ Address string }
		if json.Unmarshal(b, &w) == nil {
			a.address = w.Address
		}
	}
	if b, err := os.ReadFile(filepath.Join(a.dataDir, "settings.json")); err == nil {
		var s struct{ AutoStart bool }
		if json.Unmarshal(b, &s) == nil {
			a.autoStart = s.AutoStart
		}
	}
	go a.networkPoller()
}

func writeIfChanged(path string, data []byte, mode os.FileMode) error {
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, data) {
		return nil
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

// ---------- bound methods ----------

func (a *App) SavedAddress() string { a.mu.Lock(); defer a.mu.Unlock(); return a.address }

func (a *App) GetSettings() map[string]interface{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	return map[string]interface{}{"autoStart": a.autoStart, "hasAddress": a.address != ""}
}

func (a *App) SetAutoStart(on bool) {
	a.mu.Lock()
	a.autoStart = on
	a.mu.Unlock()
	b, _ := json.Marshal(map[string]bool{"AutoStart": on})
	_ = os.WriteFile(filepath.Join(a.dataDir, "settings.json"), b, 0o600)
}

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

// spawn starts the miner process. Caller holds a.mu.
func (a *App) spawn() error {
	dataDir := filepath.Join(a.dataDir, "data")
	var args []string
	if a.mode == "solo" {
		args = []string{"mine", "-coinbase", a.address, "-genesis", a.genPath,
			"-p2p-port", "30303", "-bootstrap", seed, "-datadir", dataDir}
	} else {
		args = []string{"mine", "-pool", poolURL, "-coinbase", a.address, "-datadir", dataDir}
	}
	cmd := exec.Command(a.binPath, args...)
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return err
	}
	a.cmd = cmd
	go a.readOutput(stdout)
	go a.watchExit(cmd)
	return nil
}

// watchExit notices if the lxs child dies on its own — a crash, a locked datadir
// (a previous orphaned solo node), a bad genesis, or a port clash. Without it the
// scanner just hits EOF and the UI keeps reporting "Mining" at the last hashrate
// forever, so the user thinks they are earning while nothing runs. An intentional
// Kill (Pause/Stop/Resume/close) replaces or nils a.cmd first, so we only flag a
// crash when the child that exited is still the current one. This is the sole
// caller of Wait(), so it also reaps the process (no zombies).
func (a *App) watchExit(cmd *exec.Cmd) {
	err := cmd.Wait()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cmd != cmd || a.status != "mining" {
		return // intentional stop/replace — not a crash
	}
	a.cmd = nil
	a.hashHs = 0
	a.status = "crashed"
	m := "Miner stopped unexpectedly"
	if err != nil {
		m += " (" + err.Error() + ")"
	}
	a.append(m + " — press Start to retry.")
}

func (a *App) StartMining(address, mode string) error {
	address = strings.TrimSpace(address)
	if !reValid.MatchString(address) {
		return fmt.Errorf("Enter a valid LXS address (0x… 40 hex), or press Create wallet.")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.status == "mining" {
		return nil
	}
	a.address, a.mode = address, mode
	a.logbuf, a.shares, a.blocks, a.hashHs, a.uptimeBase = nil, 0, 0, 0, 0
	a.append("Starting miner (" + mode + ")…")
	if err := a.spawn(); err != nil {
		return fmt.Errorf("could not start the miner: %w", err)
	}
	a.status = "mining"
	a.segStart = time.Now()
	go a.pollBalance()
	return nil
}

func (a *App) Pause() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.status != "mining" {
		return
	}
	a.killLocked()
	a.uptimeBase += time.Since(a.segStart).Seconds()
	a.status = "paused"
	a.hashHs = 0
	a.append("Paused.")
}

func (a *App) Resume() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.status != "paused" && a.status != "crashed" {
		return
	}
	a.append("Resuming…")
	if err := a.spawn(); err != nil {
		a.append("Could not resume: " + err.Error())
		return
	}
	a.status = "mining"
	a.segStart = time.Now()
}

func (a *App) Stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.killLocked()
	a.status = "idle"
	a.hashHs, a.uptimeBase = 0, 0
	a.append("Stopped.")
}

func (a *App) killLocked() {
	if a.cmd != nil && a.cmd.Process != nil {
		_ = a.cmd.Process.Kill()
	}
	a.cmd = nil
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
		st, addr := a.status, a.address
		a.mu.Unlock()
		if addr != "" {
			if v, err := rpcBalance(addr); err == nil {
				a.mu.Lock()
				a.balance = v
				a.mu.Unlock()
			}
		}
		if st == "idle" {
			return
		}
		time.Sleep(12 * time.Second)
	}
}

// networkPoller keeps the network-wide numbers fresh for the dashboard.
func (a *App) networkPoller() {
	a.fetchNetwork()
	for range time.Tick(10 * time.Second) {
		a.fetchNetwork()
	}
}

// seedPeers is how many nodes are connected to the seed right now — the live
// signal of SOLO miners (pool workers poll HTTP and never appear as peers).
func seedPeers() int {
	r, err := rpcCall("chain_miningStats", []interface{}{})
	if err != nil || r == nil {
		return 0
	}
	var d struct {
		Peers int `json:"peers"`
	}
	if json.Unmarshal(r, &d) != nil {
		return 0
	}
	return d.Peers
}

func (a *App) fetchNetwork() {
	height, diff, blkTime := chainStats()
	miners, poolHs, paid, pblocks := poolStats()
	miners += seedPeers() // live now = pool workers + solo nodes on the seed
	a.mu.Lock()
	if height > 0 {
		a.netHeight = height
		a.coinsMined = coinsMined(height)
		a.difficulty = groupThousands(fmt.Sprintf("%d", diff))
		a.diffRaw = diff
		a.blockTime = blkTime
	}
	a.minersNow, a.poolHashHs, a.poolPaid, a.poolBlocks = miners, poolHs, paid, pblocks
	a.mu.Unlock()
}

// GetState is polled by the UI (~every 2s).
func (a *App) GetState() map[string]interface{} {
	a.mu.Lock()
	defer a.mu.Unlock()

	up := a.uptimeBase
	if a.status == "mining" {
		up += time.Since(a.segStart).Seconds()
	}
	remaining := totalMined - a.coinsMined
	if remaining < 0 {
		remaining = 0
	}
	// Current block reward at this height (halves every 1,000,000 blocks) — far
	// clearer than "blocks left", since blocks don't all pay 50 LXS.
	era := a.netHeight / halving
	reward := 50.0
	for i := int64(0); i < era; i++ {
		reward /= 2
	}

	// Mode-aware stats. Miners-live-now is NETWORK-WIDE (pool workers + solo
	// nodes on the seed) and shows in both modes. Share-of-pool only exists in
	// pool mode; 0.00% in solo read as "broken" (a real user complaint).
	// Est/day and block time derive from difficulty (= expected hashes/block):
	// measuring block time from chain timestamps is garbage on a young chain
	// with idle gaps (it once showed "530 min").
	shareStr := "—"
	minersStr := fmt.Sprintf("%d", a.minersNow)
	estDay, expectSec := 0.0, 0.0
	if a.mode == "pool" {
		frac := 0.0
		if a.poolHashHs > 0 && a.hashHs > 0 {
			frac = a.hashHs / a.poolHashHs
			if frac > 1 {
				frac = 1 // the pool's average lags; never report >100%
			}
		}
		shareStr = fmt.Sprintf("%.2f%%", frac*100)
		if a.diffRaw > 0 && a.poolHashHs > 0 {
			expectSec = float64(a.diffRaw) / a.poolHashHs
			estDay = frac * reward * (86400.0 / expectSec)
		}
	} else if a.diffRaw > 0 && a.hashHs > 0 { // solo: you alone vs the difficulty
		expectSec = float64(a.diffRaw) / a.hashHs
		estDay = reward * (86400.0 / expectSec)
	}
	tail := a.logbuf
	if len(tail) > 120 {
		tail = tail[len(tail)-120:]
	}
	return map[string]interface{}{
		"status":     a.status,
		"address":    a.address,
		"mode":       a.mode,
		"balance":    a.balance,
		"hashrate":   fmtHash(a.hashHs),
		"hashRaw":    a.hashHs,
		"shares":     a.shares,
		"blocks":     a.blocks,
		"uptime":     fmtDuration(up),
		"sharePct":   shareStr,
		"estPerDay":  fmtEstDay(estDay),
		"netHeight":  groupThousands(fmt.Sprintf("%d", a.netHeight)),
		"coinsMined": fmt.Sprintf("%.0f", a.coinsMined),
		"coinsLeft":  groupThousands(fmt.Sprintf("%.0f", remaining)),
		"minedPct":   a.coinsMined / totalMined * 100,
		"reward":     rewardStr(reward),
		"minersNow":  minersStr,
		"poolHash":   fmtHash(a.poolHashHs),
		"difficulty": a.difficulty,
		"blockTime":  fmtBlockTime(expectSec),
		"poolPaid":   a.poolPaid,
		"poolBlocks": a.poolBlocks,
		"log":        strings.Join(tail, "\n"),
	}
}

// ---------- helpers ----------

func coinsMined(height int64) float64 {
	reward, era, total, h := 50.0, int64(halving), 0.0, height
	for h > 0 && reward >= 1e-9 {
		take := h
		if take > era {
			take = era
		}
		total += float64(take) * reward
		h -= take
		reward /= 2
	}
	return total
}

func fmtHash(hs float64) string {
	if hs <= 0 {
		return "—"
	}
	if hs >= 1e6 {
		return fmt.Sprintf("%.2f MH/s", hs/1e6)
	}
	if hs >= 1e3 {
		return fmt.Sprintf("%.1f kH/s", hs/1e3)
	}
	return fmt.Sprintf("%.0f H/s", hs)
}

// fmtEstDay formats estimated LXS/day: "—" when unknown, whole numbers with
// thousand grouping when large (a lone early miner sees ~14,400/day), decimals
// when small (a pool sliver).
func fmtEstDay(v float64) string {
	if v <= 0 {
		return "—"
	}
	if v >= 100 {
		return groupThousands(fmt.Sprintf("%.0f", v))
	}
	return fmt.Sprintf("%.2f", v)
}

func fmtBlockTime(s float64) string {
	if s <= 0 {
		return "—"
	}
	if s >= 60 {
		return fmt.Sprintf("%.1f min", s/60)
	}
	return fmt.Sprintf("%.0f s", s)
}

func rewardStr(r float64) string {
	if r == float64(int64(r)) {
		return fmt.Sprintf("%d LXS", int64(r))
	}
	return fmt.Sprintf("%g LXS", r)
}

func fmtDuration(sec float64) string {
	s := int64(sec)
	h := s / 3600
	m := (s % 3600) / 60
	ss := s % 60
	if h > 0 {
		return fmt.Sprintf("%dh %02dm", h, m)
	}
	if m > 0 {
		return fmt.Sprintf("%dm %02ds", m, ss)
	}
	return fmt.Sprintf("%ds", ss)
}

func groupThousands(s string) string {
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	n := len(s)
	if n <= 3 {
		if neg {
			return "-" + s
		}
		return s
	}
	var out []string
	for n > 3 {
		out = append([]string{s[n-3 : n]}, out...)
		n -= 3
	}
	out = append([]string{s[:n]}, out...)
	r := strings.Join(out, ",")
	if neg {
		return "-" + r
	}
	return r
}

// ---------- RPC ----------

func rpcCall(method string, params []interface{}) (json.RawMessage, error) {
	body, _ := json.Marshal(map[string]interface{}{"jsonrpc": "2.0", "method": method, "params": params, "id": 1})
	resp, err := http.Post(rpcURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var d struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return nil, err
	}
	return d.Result, nil
}

func rpcBalance(addr string) (string, error) {
	r, err := rpcCall("eth_getBalance", []interface{}{addr, "latest"})
	if err != nil {
		return "", err
	}
	var hex string
	json.Unmarshal(r, &hex)
	wei, ok := new(big.Int).SetString(strings.TrimPrefix(hex, "0x"), 16)
	if !ok {
		return "", fmt.Errorf("bad balance")
	}
	whole := new(big.Int).Div(wei, big.NewInt(1e18))
	frac := new(big.Int).Mod(wei, big.NewInt(1e18))
	f := fmt.Sprintf("%018d", frac)[:4]
	return fmt.Sprintf("%s.%s", groupThousands(whole.String()), f), nil
}

type blockHdr struct {
	Number     string `json:"number"`
	Timestamp  string `json:"timestamp"`
	Difficulty string `json:"difficulty"`
}

func chainStats() (height int64, difficulty int64, blockTime float64) {
	r, err := rpcCall("eth_getBlockByNumber", []interface{}{"latest", false})
	if err != nil || r == nil {
		return 0, 0, 0
	}
	var latest blockHdr
	if json.Unmarshal(r, &latest) != nil {
		return 0, 0, 0
	}
	height = hexToInt(latest.Number)
	difficulty = hexToInt(latest.Difficulty)
	// block time from a few blocks back
	if height >= 6 {
		if r2, err := rpcCall("eth_getBlockByNumber", []interface{}{fmt.Sprintf("0x%x", height-5), false}); err == nil {
			var past blockHdr
			if json.Unmarshal(r2, &past) == nil {
				dt := hexToInt(latest.Timestamp) - hexToInt(past.Timestamp)
				if dt > 0 {
					blockTime = float64(dt) / 5.0
				}
			}
		}
	}
	return
}

func poolStats() (miners int, hashHs float64, paid string, blocks int) {
	resp, err := http.Get(strings.TrimRight(poolURL, "/") + "/pool/stats")
	if err != nil {
		return 0, 0, "0", 0
	}
	defer resp.Body.Close()
	var d struct {
		MinersActive int     `json:"minersActive"`
		Hashrate     float64 `json:"hashrate"`
		TotalPaidWei string  `json:"totalPaidWei"`
		BlocksFound  int     `json:"blocksFound"`
	}
	if json.NewDecoder(resp.Body).Decode(&d) != nil {
		return 0, 0, "0", 0
	}
	paid = "0"
	if w, ok := new(big.Int).SetString(d.TotalPaidWei, 10); ok {
		paid = groupThousands(new(big.Int).Div(w, big.NewInt(1e18)).String())
	}
	return d.MinersActive, d.Hashrate, paid, d.BlocksFound
}

func hexToInt(h string) int64 {
	v, ok := new(big.Int).SetString(strings.TrimPrefix(h, "0x"), 16)
	if !ok {
		return 0
	}
	return v.Int64()
}
