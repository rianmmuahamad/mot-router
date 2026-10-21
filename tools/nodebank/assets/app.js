const $=id=>document.getElementById(id),esc=s=>String(s??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));let view='overview',cursor='',next='',lastJobs=[],sourceData=[],lastStatsAt=0,nodeRequest=0,polling=false;
async function api(path,data){const r=await fetch('/api/'+path,data===undefined?{}:{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(data)});const j=await r.json();if(!r.ok)throw Error(j.error||r.statusText);return j}
function notice(s){$('notice').textContent=s}async function action(fn){try{await fn();await refresh()}catch(e){notice(e.message)}}
function stats(s){lastStatsAt=Date.now();$('connection').textContent='VPS · terhubung';$('state').textContent=s.paused?'Dijeda':'Memeriksa';$('reason').textContent=s.pause_reason||'Antrean terbatas. Hasil disimpan di disk.';const counts=s.counts||{};$('counts').innerHTML=[['total','Total bank'],['pending','Menunggu'],['healthy','Sehat'],['failed','Gagal'],['unsupported','Unsupported']].map(([k,label])=>`<div class="card"><span>${label}</span><strong>${Number(counts[k]||0).toLocaleString('id-ID')}</strong></div>`).join('');const mb=n=>(Number(n||0)/1048576).toFixed(1);$('ram').textContent=`Engine ${mb(s.engine_rss_bytes)} MiB + worker ${mb(s.worker_rss_bytes)} MiB = ${mb(s.combined_rss_bytes)} MiB`;$('ram-meter').style.width=Math.min(100,s.combined_rss_bytes/268435456*100)+'%';$('parallel').textContent=`TCP ${s.tcp_parallelism||128} · protokol ${s.protocol_parallelism||16} · RAM tersedia ${mb(s.available_ram_bytes)} MiB`;$('speed').textContent=`Rata-rata ${Number(s.average_nodes_per_second||0).toFixed(2)} node/detik sejak engine mulai`}
function table(nodes,best=false){if(!nodes.length)return '<div class="empty">Belum ada node untuk tampilan ini.</div>';return `<div class="table-wrap"><table><thead><tr><th>Node</th><th>Status</th><th>Latensi</th><th>IP keluar</th><th>Berhasil / gagal</th><th>Diperiksa</th></tr></thead><tbody>${nodes.map(n=>`<tr><td>${esc(n.label)}<small>${esc(n.protocol)} · ${esc(n.server)}:${n.port}</small></td><td class="${esc(n.status||'healthy')}">${esc(n.status||'healthy')}<small>${esc(n.error||'')}</small></td><td>${n.latency_ms?Number(n.latency_ms).toFixed(0)+' ms':'—'}</td><td>${esc(n.exit_ip||'—')}</td><td>${n.ok} / ${n.fail}</td><td>${n.checked_at?new Date(n.checked_at*1000).toLocaleString('id-ID'):'—'}</td></tr>`).join('')}</tbody></table></div>`}
async function loadNodes(){
  const request=++nodeRequest;
  $('next-page').disabled=true;
  const q=new URLSearchParams({status:$('status-filter').value,protocol:$('protocol-filter').value,source:$('source-filter').value,after:cursor,limit:100});
  const j=await api('nodes?'+q);
  if(request!==nodeRequest)return;
  next=j.next_cursor;
  $('node-list').innerHTML=table(j.nodes);
  $('next-page').disabled=j.nodes.length<100;
}
async function sources(){sourceData=(await api('sources')).sources;$('source-list').innerHTML=sourceData.map(s=>`<div class="source"><div><strong>${esc(s.name)}</strong><p>${esc(s.url)}</p><small>${s.enabled?'Aktif':'Nonaktif'} · interval rencana ${s.interval_secs/60} menit · impor manual</small></div><div><button data-toggle="${esc(s.id)}">${s.enabled?'Nonaktifkan':'Aktifkan'}</button> <button data-import="${esc(s.id)}" ${s.enabled?'':'disabled'}>Impor</button></div></div>`).join('');const chosen=$('source-filter').value;$('source-filter').innerHTML='<option value="">Semua sumber</option>'+sourceData.map(s=>`<option value="${esc(s.id)}">${esc(s.name)}</option>`).join('');$('source-filter').value=chosen}
async function jobs(){lastJobs=(await api('jobs')).jobs;$('job-list').innerHTML=lastJobs.length?lastJobs.map(j=>`<article><strong>${esc(j.kind)} · ${esc(j.state)}</strong><p>${Number(j.processed).toLocaleString('id-ID')} diproses${j.total?' / '+Number(j.total).toLocaleString('id-ID'):''} · ${j.added} baru · ${j.ok} sehat · ${j.failed} gagal · ${j.unsupported} unsupported</p><small>${esc(j.id)}<br>${esc(j.error)}</small><p>${['scan','import'].includes(j.kind)&&['queued','running','paused','failed'].includes(j.state)?`<button data-job="${esc(j.id)}" data-action="${['paused','failed'].includes(j.state)?'resume':'pause'}">${['paused','failed'].includes(j.state)?'Lanjutkan':'Jeda'}</button> <button data-job="${esc(j.id)}" data-action="cancel">Batalkan</button>`:''}</p></article>`).join(''):'<div class="empty">Belum ada pekerjaan.</div>'}
async function refresh(){
  const tasks=[];
  // SSE already supplies live counts. Fetch a snapshot only before its first
  // event or after a disconnect; render the selected page independently.
  if(Date.now()-lastStatsAt>5000)tasks.push(api('stats').then(stats));
  if(view==='overview'||view==='jobs')tasks.push(jobs());
  if(view==='sources')tasks.push(sources());
  if(view==='bank')tasks.push(loadNodes());
  if(view==='best')tasks.push(api('best').then(j=>$('best-list').innerHTML=table(j.nodes,true)));
  await Promise.all(tasks);
}
document.querySelectorAll('nav button').forEach(b=>b.onclick=()=>action(async()=>{view=b.dataset.view;document.querySelectorAll('.view').forEach(v=>v.hidden=v.id!==view);document.querySelectorAll('nav button').forEach(v=>v.classList.toggle('active',v===b));$('title').textContent=b.textContent;}));
$('start').onclick=()=>action(async()=>{const j=await api('scan',{background:$('background').checked});notice('Pemeriksaan dimulai · '+j.job_id)});$('pause').onclick=()=>action(async()=>{const j=lastJobs.find(j=>j.kind==='scan'&&j.state==='running');if(j)await api('jobs/'+j.id+'/control',{action:'pause'})});$('resume').onclick=()=>action(async()=>{const j=lastJobs.find(j=>j.kind==='scan'&&['paused','running'].includes(j.state));if(!j)throw Error('Mulai pekerjaan baru terlebih dahulu.');await api('jobs/'+j.id+'/control',{action:'resume'});notice('Pemeriksaan dilanjutkan')});
$('upload').onsubmit=e=>{e.preventDefault();action(async()=>{const r=await fetch('/api/upload',{method:'POST',body:new FormData(e.target)});const j=await r.json();if(!r.ok)throw Error(j.error);notice('Impor dijadwalkan · '+j.job_id)})};$('source-form').onsubmit=e=>{e.preventDefault();action(async()=>{const f=new FormData(e.target);await api('sources',{id:'',name:f.get('name'),url:f.get('url'),enabled:false,interval_secs:3600,last_fetch:0});e.target.reset();notice('Sumber disimpan dalam keadaan nonaktif')})};
$('source-list').onclick=e=>action(async()=>{if(e.target.dataset.toggle){const s=sourceData.find(s=>s.id===e.target.dataset.toggle);await api('sources',{...s,enabled:!s.enabled})}if(e.target.dataset.import){const j=await api('sources/'+encodeURIComponent(e.target.dataset.import)+'/import',{});notice('Mengunduh subscription · '+j.job_id)}});
$('job-list').onclick=e=>{if(e.target.dataset.job)action(()=>api('jobs/'+e.target.dataset.job+'/control',{action:e.target.dataset.action}))};['status-filter','protocol-filter','source-filter'].forEach(id=>$(id).onchange=()=>action(async()=>{cursor=''}));$('reset-page').onclick=()=>action(async()=>{cursor=''});$('next-page').onclick=()=>action(async()=>{cursor=next});$('export').onclick=()=>action(async()=>{const j=await api('export',{});notice('Ekspor dijadwalkan · '+j.job_id)});$('download').onclick=()=>action(async()=>{const r=await fetch('/api/export/download',{headers:{'X-NodeBank-Export':'1'}});if(!r.ok){const j=await r.json();throw Error(j.error)}const link=document.createElement('a');link.href=URL.createObjectURL(await r.blob());link.download='healthy-v1.json';link.click();setTimeout(()=>URL.revokeObjectURL(link.href),1000)});

const events=new EventSource('/api/events');events.addEventListener('stats',e=>stats(JSON.parse(e.data)));events.onerror=()=>{$('connection').textContent='Menghubungkan ulang…'};Promise.all([sources(),api('catalog').then(items=>{
  $('catalog').innerHTML=items.map(s=>`<p>${esc(s.name)} · <a href="${esc(s.url)}" target="_blank" rel="noreferrer">${esc(s.url)}</a></p>`).join('');
}),refresh()]).catch(e=>notice(e.message));
setInterval(async()=>{
  if(polling||document.hidden||!['jobs','best'].includes(view))return;
  polling=true;
  try{await refresh()}catch(e){notice(e.message)}finally{polling=false}
},5000);
