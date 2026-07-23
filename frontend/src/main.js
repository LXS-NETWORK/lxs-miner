import './style.css';
import { SavedAddress, CreateWallet, StartMining, StopMining, GetState } from '../wailsjs/go/main/App';

const $ = (id) => document.getElementById(id);
let inMiningView = false;

// pre-fill a remembered address
SavedAddress().then((a) => { if (a) $('addr').value = a; }).catch(() => {});

$('create').addEventListener('click', async () => {
  $('create').disabled = true;
  try {
    const w = await CreateWallet();
    $('addr').value = w.address;
    $('m-addr').textContent = w.address;
    $('m-key').textContent = w.key;
    $('m-path').textContent = w.path;
    $('m-copy').onclick = () => copy(w.key, $('m-copy'));
    $('modal').style.display = 'flex';
  } catch (e) { alert('' + e); }
  $('create').disabled = false;
});
$('m-ok').addEventListener('click', () => { $('modal').style.display = 'none'; });

$('start').addEventListener('click', async () => {
  const addr = $('addr').value.trim();
  const mode = document.querySelector('input[name=mode]:checked').value;
  $('start').disabled = true;
  try {
    await StartMining(addr, mode);
    inMiningView = true; view();
  } catch (e) { alert('' + e); }
  $('start').disabled = false;
});

$('stop').addEventListener('click', async () => {
  const st = await GetState().catch(() => ({ running: false }));
  if (st.running) StopMining();
  else { inMiningView = false; view(); }
});

function view() {
  $('setup').style.display = inMiningView ? 'none' : '';
  $('mining').style.display = inMiningView ? '' : 'none';
}

function copy(txt, btn) {
  const t = document.createElement('textarea');
  t.value = txt; document.body.appendChild(t); t.select();
  try { document.execCommand('copy'); } catch (e) {}
  try { if (navigator.clipboard) navigator.clipboard.writeText(txt); } catch (e) {}
  t.remove();
  const o = btn.textContent; btn.textContent = 'Copied ✓';
  setTimeout(() => { btn.textContent = o; }, 1400);
}

// live state poll
setInterval(async () => {
  let s;
  try { s = await GetState(); } catch (e) { return; }
  $('balance').textContent = s.balance;
  $('hashrate').textContent = s.hashrate;
  $('shares').textContent = s.shares;
  $('blocks').textContent = s.blocks;
  if (s.address) $('balfor').textContent = s.address.slice(0, 10) + '…' + s.address.slice(-4);
  const log = $('log');
  const atBottom = log.scrollHeight - log.scrollTop - log.clientHeight < 40;
  if (log.textContent !== s.log) { log.textContent = s.log; if (atBottom) log.scrollTop = log.scrollHeight; }
  if (s.running) {
    $('status').textContent = '● Mining (' + s.mode + ')'; $('status').className = 'ok'; $('stop').textContent = '■ Stop';
  } else {
    $('status').textContent = '● Stopped'; $('status').className = 'stop'; $('stop').textContent = '← Back';
  }
}, 2000);
