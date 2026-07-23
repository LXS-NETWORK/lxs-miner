import './style.css';
import { SavedAddress, CreateWallet, StartMining, Pause, Resume, Stop, GetState, GetSettings, SetAutoStart }
  from '../wailsjs/go/main/App';

const $ = (id) => document.getElementById(id);
let samples = [];
let prevBalance = null, prevBlocks = 0;

const modeVal = () => (document.querySelector('input[name=mode]:checked') || {}).value || 'pool';

/* ---------- splash → app ---------- */
setTimeout(async () => {
  const sp = $('splash'); if (sp) sp.remove();
  $('mainapp').style.display = '';
  const s = await GetSettings().catch(() => ({}));
  const addr = await SavedAddress().catch(() => '');
  if (addr) $('addr').value = addr;
  if (s.autoStart) $('autostart').checked = true;
  if (s.autoStart && addr) {
    try { await StartMining(addr, 'pool'); goDash(); } catch (e) { showSetup(); }
  } else { showSetup(); }
}, 5000);

function showSetup(){ $('setup').style.display=''; $('createpage').style.display='none'; $('dash').style.display='none'; }
function showCreate(){ $('setup').style.display='none'; $('createpage').style.display=''; $('dash').style.display='none'; }
function goDash(){ $('setup').style.display='none'; $('createpage').style.display='none'; $('dash').style.display=''; }

/* ---------- setup ---------- */
$('start').addEventListener('click', async () => {
  const a = $('addr').value.trim();
  if (!/^0x[0-9a-fA-F]{40}$/.test(a)) { alert('Enter a valid LXS address (0x…), or press Create wallet.'); return; }
  $('start').disabled = true;
  try { await StartMining(a, modeVal()); goDash(); } catch (e) { alert('' + e); }
  $('start').disabled = false;
});

$('createbtn').addEventListener('click', async () => {
  $('createbtn').disabled = true;
  try {
    const w = await CreateWallet();
    $('cw-addr').textContent = w.address;
    $('cw-key').textContent = w.key;
    $('cw-path').textContent = w.path;
    $('cw-copy').onclick = () => copy(w.key, $('cw-copy'));
    $('cw-done').onclick = () => { $('addr').value = w.address; showSetup(); };
    showCreate();
  } catch (e) { alert('' + e); }
  $('createbtn').disabled = false;
});
$('cw-back').addEventListener('click', showSetup);

/* ---------- dashboard controls ---------- */
$('pause').addEventListener('click', () => Pause());
$('resume').addEventListener('click', () => Resume());
$('stop').addEventListener('click', () => { Stop(); showSetup(); });
$('copy').addEventListener('click', () => copy($('addr').value.trim(), $('copy')));
$('autostart').addEventListener('change', (e) => SetAutoStart(e.target.checked));

function copy(txt, btn){
  const t = document.createElement('textarea'); t.value = txt; document.body.appendChild(t); t.select();
  try { document.execCommand('copy'); } catch(e){}
  try { if (navigator.clipboard) navigator.clipboard.writeText(txt); } catch(e){}
  t.remove(); const o = btn.textContent; btn.textContent = '✓ Copied'; setTimeout(() => btn.textContent = o, 1400);
}

/* ---------- live poll ---------- */
setInterval(async () => {
  let s; try { s = await GetState(); } catch(e){ return; }

  $('balance').textContent = s.balance;
  $('balfor').textContent = s.address ? '· ' + s.address.slice(0,8) + '…' + s.address.slice(-4) : '';
  $('hashnow').textContent = s.hashrate;
  $('s-blocks').textContent = s.blocks;
  $('s-share').textContent = s.sharePct;
  $('s-est').textContent = s.estPerDay;
  $('s-miners').textContent = s.minersNow;
  $('s-diff').textContent = s.difficulty;
  $('s-btime').textContent = s.blockTime;
  $('uptime').textContent = (s.status !== 'idle') ? '⏱ ' + s.uptime : '';
  $('n-mined').textContent = fmtM(s.coinsMined);
  $('n-left').textContent = s.coinsLeft;
  $('n-height').textContent = s.netHeight;
  $('n-bleft').textContent = s.blocksLeft;
  $('n-paid').textContent = s.poolPaid + ' LXS';
  $('n-bar').style.width = Math.max(0.5, s.minedPct) + '%';

  const st = s.status;
  $('status').textContent = st === 'mining' ? '● Mining (' + s.mode + ')' : st === 'paused' ? '● Paused' : '● Idle';
  $('status').className = st === 'mining' ? 'ok' : st === 'paused' ? 'warn' : 'stop';
  $('pause').style.display  = st === 'mining' ? '' : 'none';
  $('resume').style.display = st === 'paused' ? '' : 'none';

  const log = $('log');
  const atBottom = log.scrollHeight - log.scrollTop - log.clientHeight < 40;
  if (log.textContent !== s.log) { log.textContent = s.log; if (atBottom) log.scrollTop = log.scrollHeight; }

  if (st === 'mining') { samples.push(s.hashRaw || 0); if (samples.length > 60) samples.shift(); drawChart(); }

  const bal = parseFloat((s.balance || '0').replace(/,/g,''));
  if (prevBalance !== null && bal > prevBalance + 0.0001) { toast('💰 You got paid +' + (bal - prevBalance).toFixed(4) + ' LXS'); beep(); }
  prevBalance = bal;
  if (s.blocks > prevBlocks) { toast('⛏ Block found! (#' + s.blocks + ')'); beep(); }
  prevBlocks = s.blocks;
}, 2000);

/* ---------- hashrate chart ---------- */
function drawChart(){
  const c = $('chart'); if (!c) return; const ctx = c.getContext('2d');
  const W = c.width, H = c.height; ctx.clearRect(0,0,W,H);
  if (samples.length < 2) return;
  const max = Math.max(...samples, 1);
  const step = W / (samples.length - 1);
  const y = v => H - 6 - (v / max) * (H - 14);
  const g = ctx.createLinearGradient(0,0,0,H);
  g.addColorStop(0,'rgba(70,229,161,.30)'); g.addColorStop(1,'rgba(58,160,255,.02)');
  ctx.beginPath(); ctx.moveTo(0,H);
  samples.forEach((v,i)=>ctx.lineTo(i*step,y(v)));
  ctx.lineTo((samples.length-1)*step,H); ctx.closePath(); ctx.fillStyle=g; ctx.fill();
  const lg = ctx.createLinearGradient(0,0,W,0);
  lg.addColorStop(0,'#46e5a1'); lg.addColorStop(1,'#3aa0ff');
  ctx.beginPath(); samples.forEach((v,i)=>i?ctx.lineTo(i*step,y(v)):ctx.moveTo(0,y(v)));
  ctx.strokeStyle=lg; ctx.lineWidth=2; ctx.lineJoin='round'; ctx.stroke();
}

/* ---------- toast + beep ---------- */
function toast(msg){
  const t = document.createElement('div'); t.className='toast'; t.textContent=msg;
  $('toasts').appendChild(t); setTimeout(()=>t.remove(), 4200);
}
let actx;
function beep(){
  try{
    actx = actx || new (window.AudioContext||window.webkitAudioContext)();
    const o=actx.createOscillator(), g=actx.createGain();
    o.type='sine'; o.frequency.value=880; o.connect(g); g.connect(actx.destination);
    g.gain.setValueAtTime(.08,actx.currentTime); g.gain.exponentialRampToValueAtTime(.0001,actx.currentTime+.25);
    o.start(); o.stop(actx.currentTime+.26);
  }catch(e){}
}
function fmtM(s){ const n = parseFloat(s); if (isNaN(n)) return s; if (n>=1e6) return (n/1e6).toFixed(2)+'M'; return Math.round(n).toLocaleString(); }
