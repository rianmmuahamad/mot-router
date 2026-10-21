'use strict';

const paths = {
  overview:'<rect x="3" y="3" width="7" height="7" rx="1.5"/><rect x="14" y="3" width="7" height="7" rx="1.5"/><rect x="3" y="14" width="7" height="7" rx="1.5"/><rect x="14" y="14" width="7" height="7" rx="1.5"/>',
  users:'<path d="M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2m20 0v-2a4 4 0 0 0-3-3.87M15 3.13a4 4 0 0 1 0 7.75"/><circle cx="9" cy="7" r="4"/>',
  shield:'<path d="M12 22s8-4 8-11V5l-8-3-8 3v6c0 7 8 11 8 11zM12 8v4m0 4h.01"/>',
  code:'<path d="m16 5 6 7-6 7M8 5l-6 7 6 7m6-16-4 20"/>',
  layers:'<path d="m12 3 10 5-10 5L2 8l10-5zm-10 9 10 5 10-5M2 16l10 5 10-5"/>',
  sparkles:'<path d="m12 3 2.7 6.3L21 12l-6.3 2.7L12 21l-2.7-6.3L3 12l6.3-2.7L12 3zM20 2v4m-2-2h4"/>',
  'chevron-right':'<path d="m9 5 7 7-7 7"/>',
  'chevron-left':'<path d="m15 5-7 7 7 7"/>',
  'arrow-right':'<path d="M4 12h16m-6-6 6 6-6 6"/>',
  'arrow-up-right':'<path d="M7 17 17 7M7 7h10v10"/>',
  menu:'<path d="M3 6h18M3 12h18M3 18h18"/>',
  bell:'<path d="M18 8a6 6 0 0 0-12 0c0 7-3 7-3 9h18c0-2-3-2-3-9m-11 12a2 2 0 0 0 4 0"/>',
  download:'<path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4m9-12v12m-5-5 5 5 5-5"/>',
  plus:'<path d="M12 5v14M5 12h14"/>',
  search:'<circle cx="10.5" cy="10.5" r="7.5"/><path d="m16 16 5 5"/>',
  receipt:'<path d="M6 3h12v18l-3-2-3 2-3-2-3 2V3zm3 4h6m-6 4h6m-6 4h3"/>',
  info:'<circle cx="12" cy="12" r="9"/><path d="M12 11v6m0-10h.01"/>',
  copy:'<rect x="8" y="8" width="13" height="13" rx="2"/><path d="M16 8V3H3v13h5"/>',
  x:'<path d="m6 6 12 12M6 18 18 6"/>',
  'check-circle':'<circle cx="12" cy="12" r="9"/><path d="m8 12 3 3 5-6"/>',
  infinity:'<path d="M12 12s-3-5-6-5a5 5 0 0 0 0 10c3 0 6-5 6-5s3-5 6-5a5 5 0 0 1 0 10c-3 0-6-5-6-5z"/>',
  clock:'<circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/>',
  activity:'<path d="M2 12h5l3-8 4 16 3-8h5"/>',
  key:'<circle cx="8" cy="8" r="5"/><path d="m12 12 9 9m-4-4 3-3m-6 0 3-3"/>',
  refresh:'<path d="M20 8a8 8 0 0 0-14-3L3 8m0-5v5h5m-4 8a8 8 0 0 0 14 3l3-3m0 5v-5h-5"/>'
};
const icon = name => `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.65" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${paths[name] || paths.overview}</svg>`;
function hydrateIcons(root=document) { root.querySelectorAll('[data-icon]').forEach(el => el.innerHTML=icon(el.dataset.icon)); }
const $ = id => document.getElementById(id);
const esc = value => String(value ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const nf = new Intl.NumberFormat('id-ID');
const compact = value => value >= 1e6 ? `${new Intl.NumberFormat('id-ID',{maximumFractionDigits:2}).format(value/1e6)}M` : value >= 1e3 ? `${new Intl.NumberFormat('id-ID',{maximumFractionDigits:1}).format(value/1e3)}K` : nf.format(value);
const dayMs = 86400000;
const date = value => value ? new Intl.DateTimeFormat('id-ID',{day:'numeric',month:'short',year:'numeric',timeZone:'Asia/Jakarta'}).format(new Date(value)) : 'Tanpa batas waktu';
const palette=['lilac','mint','sand','blue'];
let accounts=[], overview={}, usage={buckets:[]}, apiBase='', clockOffset=0, loaded=false;
let activeFilter='all', page=1, editorMode='create', editingId=null, reviewingId=null, toastTimer, loadPromise=null, mutationBusy=false, dataRevision=0;
const perPage=10;
const currentTime=()=>Date.now()+clockOffset;
function normalize(u) {
  return {id:u.id,name:u.name,plan:u.plan_type||'unlimited',limit:u.token_limit||0,used:u.quota_used||0,
    input:u.tokens_in||0,output:u.tokens_out||0,requests:u.request_count||0,expires:u.expires_at,created:u.created_at,
    key:u.key,url:u.url,apiBase:u.api_base,revoked:!!u.revoked,review:u.quota_state==='unknown',running:u.quota_state==='inflight',
    issue:u.quota_issue,pending:u.quota_pending_request_id};
}
function status(u) {
  if(u.revoked)return 'revoked';
  if(u.expires && u.expires<=currentTime())return 'expired';
  if(u.review)return 'review';
  if(u.running)return 'running';
  if(u.plan==='token' && u.used>=u.limit)return 'exhausted';
  return 'active';
}
const statuses={active:'Aktif',running:'Request berjalan',review:'Review usage',expired:'Kedaluwarsa',revoked:'Dicabut',exhausted:'Kuota habis'};
const isActive=u=>['active','running'].includes(status(u));
const badge=u=>`<span class="badge ${status(u)}">${statuses[status(u)]}</span>`;
const avatar=u=>`<span class="avatar ${palette[(u.id-1)%palette.length]}">${esc(u.name.trim().split(/\s+/).slice(0,2).map(s=>s[0]||'A').join('').toUpperCase())}</span>`;
const maskedKey=u=>`${u.key.slice(0,3)}••••${u.key.slice(-4)}`;
function remaining(u) {
  if(!u.expires)return 'Tanpa tanggal berakhir';
  const seconds=Math.floor((u.expires-currentTime())/1000);
  if(seconds<=0)return 'Masa aktif berakhir';
  if(seconds>=86400)return `${Math.ceil(seconds/86400)} hari tersisa`;
  if(seconds>=3600)return `${Math.ceil(seconds/3600)} jam tersisa`;
  return `${Math.max(1,Math.ceil(seconds/60))} menit tersisa`;
}
function notify(message) {
  $('toastText').textContent=message;$('toast').hidden=false;clearTimeout(toastTimer);toastTimer=setTimeout(()=>$('toast').hidden=true,4500);
}
async function copyText(value) {
  if(!value){notify('Data belum tersedia.');return;}
  try{await navigator.clipboard.writeText(value);notify('Berhasil disalin.');}
  catch{notify('Browser memblokir penyalinan. Pilih dan salin teks secara manual.');}
}
async function api(path,{method='GET',body}={}) {
  const controller=new AbortController(),timer=setTimeout(()=>controller.abort(),10000);
  try{
    const options={method,credentials:'same-origin',cache:'no-store',signal:controller.signal};
    if(body!==undefined){options.headers={'Content-Type':'application/json'};options.body=JSON.stringify(body);}
    const response=await fetch(path,options),data=await response.json();
    if(!response.ok)throw new Error(response.status===401?'Sesi admin belum terautentikasi. Muat ulang halaman dan login kembali.':data.error?.message||'Permintaan tidak berhasil.');
    return data;
  }catch(error){if(error.name==='AbortError')throw new Error('Portal terlalu lama merespons. Coba perbarui data.');throw error;}
  finally{clearTimeout(timer);}
}
async function mutate(path,body,method='POST') {
  if(mutationBusy)throw new Error('Tunggu sampai tindakan sebelumnya selesai.');
  mutationBusy=true;
  dataRevision++;
  try{return await api(path,{method,body});}finally{mutationBusy=false;}
}
function upsert(raw) {const u=normalize(raw),i=accounts.findIndex(a=>a.id===u.id);if(i<0)accounts.unshift(u);else accounts[i]=u;render();}
async function load() {
  if(mutationBusy)return false;
  if(loadPromise)return loadPromise;
  const revision=dataRevision;
  $('refreshButton').disabled=true;$('retryLoad').disabled=true;
  loadPromise=(async()=>{
    try{
      const [users,ov,chart]=await Promise.all([api('/api/admin/users'),api('/api/admin/overview'),api('/api/admin/usage?period=7d')]);
      if(revision!==dataRevision)return false;
      accounts=users.map(normalize);overview=ov;usage=chart;apiBase=ov.api_base||'';clockOffset=ov.server_time-Date.now();loaded=true;
      $('loadError').hidden=true;$('connectionStatus').textContent='Data terbaru';
      document.querySelector('.preview-pill').classList.add('live');document.querySelector('.preview-pill').classList.remove('failed');
      $('apiBaseText').textContent=apiBase;$('curlExample').textContent=curlExample();render();renderChart();return true;
    }catch(error){
      $('loadErrorText').textContent=(loaded?'Data terakhir tetap ditampilkan. ':'')+error.message;
      $('loadError').hidden=false;$('connectionStatus').textContent='Koneksi bermasalah';
      document.querySelector('.preview-pill').classList.add('failed');document.querySelector('.preview-pill').classList.remove('live');
      return false;
    }finally{$('refreshButton').disabled=false;$('retryLoad').disabled=false;loadPromise=null;}
  })();
  return loadPromise;
}
async function refreshAfterMutation(){if(loadPromise)await loadPromise;return load();}
function renderStats() {
  const active=accounts.filter(isActive).length,tokenAccounts=accounts.filter(u=>u.plan==='token').length;
  const total=accounts.reduce((s,u)=>s+u.input+u.output,0),requests=accounts.reduce((s,u)=>s+u.requests,0);
  $('stats').innerHTML=[
    ['Total akun',nf.format(accounts.length),`<strong>${active} aktif</strong> · ${accounts.length-active} perlu perhatian`,'users'],
    ['Token terpakai',compact(total),'Input + output · seluruh akun','activity'],
    ['Total request',nf.format(requests),'Seluruh masa aktif akun','layers'],
    ['Akun paket token',nf.format(tokenAccounts),`${accounts.length-tokenAccounts} akun Unlimited`,'receipt']
  ].map(([title,value,foot,ico])=>`<article class="stat"><div class="stat-top"><span>${title}</span>${icon(ico)}</div><strong class="stat-value ${loaded?'':'loading-number'}">${loaded?value:'—'}</strong><p class="stat-foot">${loaded?foot:'Memuat data…'}</p></article>`).join('');
  $('accountCount').textContent=loaded?accounts.length:'—';$('reviewCount').textContent=loaded?accounts.filter(u=>u.review).length:'—';
  document.querySelector('#notificationButton i').hidden=!accounts.some(u=>u.review);$('exportButton').disabled=!loaded;
}
function renderChart() {
  const buckets=usage.buckets,input=buckets.map(b=>b.tokens_in),output=buckets.map(b=>b.tokens_out);
  const total=buckets.reduce((s,b)=>s+b.tokens_in+b.tokens_out,0);
  $('weeklyTotal').textContent=loaded?compact(total):'—';
  $('chartNote').textContent=usage.unverified_requests?`${nf.format(usage.unverified_requests)} laporan belum terverifikasi · belum dihitung`:'Berdasarkan laporan token provider';
  const max=Math.max(1,...input,...output),ceiling=max<=4?4:Math.ceil(max/4)*4;
  const x=i=>48+i*112,y=v=>170-v/ceiling*132,points=values=>values.map((v,i)=>`${x(i)},${y(v)}`).join(' ');
  const weekdays=buckets.map(b=>new Intl.DateTimeFormat('id-ID',{weekday:'short',timeZone:'Asia/Jakarta'}).format(new Date(b.timestamp)));
  $('usageChart').innerHTML=`<defs><linearGradient id="chartFill" x1="0" y1="0" x2="0" y2="1"><stop offset="0%" stop-color="#111111" stop-opacity=".06"/><stop offset="100%" stop-color="#111111" stop-opacity="0"/></linearGradient></defs>`+
    [0,1,2,3,4].map(i=>{const v=i*ceiling/4;return `<line x1="48" x2="720" y1="${y(v)}" y2="${y(v)}" stroke="#eeeeee" stroke-dasharray="3 5"/><text x="4" y="${y(v)+4}" fill="#898989" font-size="10" font-family="Inter,Arial">${compact(v)}</text>`;}).join('')+
    (total?`<polygon points="48,170 ${points(input)} 720,170" fill="url(#chartFill)"/><polyline points="${points(output)}" fill="none" stroke="#a4acb8" stroke-width="2" stroke-linejoin="round"/><polyline points="${points(input)}" fill="none" stroke="#111111" stroke-width="2" stroke-linejoin="round"/>`+
    input.map((v,i)=>`<circle cx="${x(i)}" cy="${y(v)}" r="3" fill="white" stroke="#111111" stroke-width="1.5"><title>${date(buckets[i].timestamp)}: input ${nf.format(v)}, output ${nf.format(output[i])}</title></circle>`).join(''):
    `<text x="384" y="105" text-anchor="middle" fill="#898989" font-size="12" font-family="Inter,Arial">${loaded?'Belum ada token tercatat dalam periode ini.':'Memuat penggunaan token…'}</text>`)+
    weekdays.map((d,i)=>`<text x="${x(i)}" y="199" text-anchor="middle" fill="#898989" font-size="10" font-family="Inter,Arial">${d}</text>`).join('');
}
function renderAttention() {
  const needs=accounts.filter(u=>u.review).slice(0,2),expired=accounts.filter(u=>status(u)==='expired').slice(0,1);
  $('attentionList').innerHTML=[...needs,...expired].map(u=>`<div class="attention-item ${u.review?'':'expiry'}"><span>${icon(u.review?'shield':'clock')}</span><div><button data-action="${u.review?'review':'detail'}" data-id="${u.id}"><strong>${esc(u.name)}</strong><p>${u.review?'Laporan usage belum lengkap.':'Masa aktif akun telah berakhir.'}</p><small>${u.review?'Request berikutnya ditahan.':'Tambahkan hari untuk memperpanjang.'}</small></button></div></div>`).join('')||`<div class="attention-item"><span>${icon(loaded?'check-circle':'refresh')}</span><div><strong>${loaded?'Semua sudah beres.':'Memuat akun…'}</strong><p>${loaded?'Tidak ada laporan tertunda atau akun kedaluwarsa.':'Status akun sedang diperiksa.'}</p></div></div>`;
}
function renderAccounts() {
  const search=$('searchInput').value.trim().toLowerCase(),plan=$('planFilter').value;
  const filtered=accounts.filter(u=>(!search||u.name.toLowerCase().includes(search))&&(plan==='all'||u.plan===plan)&&(activeFilter==='all'||(activeFilter==='active'?isActive(u):!isActive(u))));
  const totalPages=Math.max(1,Math.ceil(filtered.length/perPage));page=Math.min(page,totalPages);$('tableCount').textContent=loaded?accounts.length:'—';
  $('accountRows').innerHTML=filtered.slice((page-1)*perPage,page*perPage).map(u=>{
    const percent=u.plan==='token'&&u.limit?Math.min(100,Math.round(u.used/u.limit*100)):0;
    const quota=u.plan==='token'?`<div class="quota-line"><strong>${compact(u.used)} / ${compact(u.limit)}</strong><span>${percent}%</span></div><div class="progress ${percent>=90?'low':''}"><span style="width:${percent}%"></span></div><small class="cell-sub">${compact(Math.max(0,u.limit-u.used))} token tersisa</small>`:`<span class="unlimited-quota">${icon('infinity')}Tanpa batas token</span><small class="cell-sub">${compact(u.input+u.output)} token terpakai</small>`;
    return `<tr><td><div class="user-cell">${avatar(u)}<div><strong>${esc(u.name)}</strong><small>${esc(maskedKey(u))}</small></div></div></td><td><span class="plan-name">${u.plan==='token'?'Kuota token':'Unlimited'}</span><small class="cell-sub">${u.plan==='token'?compact(u.limit)+' token':'Akses tanpa batas'}${u.expires?' · '+Math.max(1,Math.ceil((u.expires-u.created)/dayMs))+' hari':''}</small></td><td class="quota-cell">${quota}</td><td>${badge(u)}</td><td>${date(u.expires)}<small class="cell-sub">${remaining(u)}</small></td><td class="align-right">${nf.format(u.requests)}</td><td><button class="manage-button" data-action="detail" data-id="${u.id}">Kelola${icon('chevron-right')}</button></td></tr>`;
  }).join('')||`<tr><td colspan="7" class="empty-row">${loaded?accounts.length?'Tidak ada akun yang cocok. Coba kata kunci atau filter lain.':'Belum ada akun. Buat akun pertama untuk mulai.':'Memuat akun…'}</td></tr>`;
  $('resultsLabel').textContent=filtered.length?`Menampilkan ${(page-1)*perPage+1}–${Math.min(page*perPage,filtered.length)} dari ${filtered.length} akun`:loaded?'0 akun ditemukan':'Memuat…';
  $('pageLabel').textContent=`${page} / ${totalPages}`;$('prevPage').disabled=page<=1;$('nextPage').disabled=page>=totalPages;
}
function renderReview() {
  $('reviewCards').innerHTML=accounts.filter(u=>u.review).map(u=>{
    const q=u.issue||{};
    return `<article class="panel review-card"><div class="user-cell">${avatar(u)}<div><strong>${esc(u.name)}</strong><small>Request ${esc(u.pending.slice(0,12))}…</small></div><span style="margin-left:auto"><span class="badge review">Review usage</span></span></div><p>${esc(q.reason||'Laporan token final belum tersedia.')}</p><dl><div><dt>Model</dt><dd>${esc(q.model||'Tidak tersedia')}</dd></div><div><dt>Endpoint</dt><dd>${esc(q.surface||'Tidak tersedia')}</dd></div><div><dt>Laporan parsial input</dt><dd>${nf.format(q.partial_input_tokens||0)} token</dd></div><div><dt>Laporan parsial output</dt><dd>${nf.format(q.partial_output_tokens||0)} token</dd></div></dl><button class="button primary" data-action="review" data-id="${u.id}">${icon('shield')}Tinjau laporan</button><p class="status-description">Laporan parsial belum dibebankan ke kuota.</p></article>`;
  }).join('')||`<article class="panel review-card"><span class="large-icon">${icon('check-circle')}</span><h2>${loaded?'Tidak ada usage tertunda.':'Memuat laporan…'}</h2><p>${loaded?'Setiap request berikutnya mengikuti kuota dan masa aktif akun.':'Laporan token sedang diperiksa.'}</p></article>`;
}
function render(){renderStats();renderAccounts();renderAttention();renderReview();}
const views={ringkasan:['Ringkasan','Semua dalam kendali.','Pantau penggunaan dan kelola akses AI dari satu tempat.'],akun:['Akun & paket','Paket fleksibel. Akses terkendali.','Tentukan kuota token dan masa aktif untuk setiap user.'],review:['Review usage','Pastikan setiap token tercatat.','Selesaikan laporan usage yang belum terverifikasi.'],integrasi:['Integrasi API','Siap terhubung.','Satu base URL untuk aplikasi dan AI agent user kamu.']};
function navigate(){
  const hash=location.hash.slice(1),view=Object.hasOwn(views,hash)?hash:'ringkasan';
  const [crumb,title,sub]=views[view];$('crumb').textContent=crumb;$('pageTitle').textContent=title;$('pageSubtitle').textContent=sub;
  document.querySelectorAll('[data-view]').forEach(el=>{const active=el.dataset.view===view;el.classList.toggle('active',active);if(active)el.setAttribute('aria-current','page');else el.removeAttribute('aria-current');});
  $('overviewSection').hidden=view!=='ringkasan';$('accountsSection').hidden=!['ringkasan','akun'].includes(view);$('stats').hidden=!['ringkasan','akun'].includes(view);$('reviewSection').hidden=view!=='review';$('integrationSection').hidden=view!=='integrasi';$('exportButton').hidden=!['ringkasan','akun'].includes(view);
  $('sidebar').classList.remove('open');$('menuButton').setAttribute('aria-expanded','false');
}
function tokensFromM(value,optional=false){
  const text=value.trim();if(optional&&!text)return 0;
  if(!/^(?:\d+(?:\.\d{1,6})?|\.\d{1,6})$/.test(text))throw new Error('Isi jumlah token dalam M, maksimal enam angka desimal.');
  const [whole,fraction='']=text.split('.');const tokens=BigInt(whole||'0')*1000000n+BigInt(fraction.padEnd(6,'0')||'0');
  if(tokens>BigInt(Number.MAX_SAFE_INTEGER))throw new Error('Jumlah token melebihi batas.');
  if(tokens<=0n)throw new Error('Jumlah token harus lebih besar dari nol.');return Number(tokens);
}
function syncForm(){
  const isToken=$('planType').value==='token';$('tokenField').hidden=!isToken;$('tokenInput').disabled=!isToken;$('tokenInput').required=isToken&&editorMode==='create';
  $('packageDescription').innerHTML=`<span>${icon(isToken?'receipt':'infinity')}</span><div><strong>${isToken?'Kuota sesuai kebutuhan':'Unlimited selama masa aktif'}</strong><p>${isToken?'Kuota mencakup input + output, tanpa reset harian.':'Tanpa batas token. Akses tetap mengikuti durasi paket.'}</p></div>`;
  $('durationHelp').textContent=editorMode==='topup'?'Tambahan 1–365 hari. Boleh dikosongkan.':isToken?'1–365 hari, tanpa reset kuota harian.':'1–365 hari masa akses Unlimited.';
  $('formInfo').textContent=editorMode==='topup'?'Isi ulang menambah sisa kuota. Durasi ditambahkan dari akhir masa aktif, atau dari sekarang jika sudah kedaluwarsa.':isToken?'Pemakaian input dan output dihitung dengan bobot sama. Kuota berlaku selama seluruh masa aktif.':'User dapat mengirim request selama masa aktif akun. Akses berhenti ketika durasi habis atau akun dicabut.';
  $('summaryPlan').textContent=isToken?'Kuota token':'Unlimited';$('summaryTokenRow').hidden=!isToken;
  let tokens=0;try{tokens=tokensFromM($('tokenInput').value,true);}catch{}
  const days=Number($('daysInput').value)||0,u=accounts.find(a=>a.id===editingId),base=editorMode==='topup'&&u?Math.max(currentTime(),u.expires):currentTime();
  $('summaryTokens').textContent=tokens?`${editorMode==='topup'?'+ ':''}${nf.format(tokens)} token`:editorMode==='topup'?'Tidak ditambah':'Belum diisi';
  $('summaryDays').textContent=days?`${editorMode==='topup'?'+ ':''}${days} hari`:editorMode==='topup'?'Tidak ditambah':'Belum diisi';$('summaryExpiry').textContent=days?date(base+days*dayMs):editorMode==='topup'&&u?date(u.expires):'—';
}
function openEditor(id=null){
  if(mutationBusy){notify('Tunggu sampai tindakan sebelumnya selesai.');return;}
  $('accountForm').reset();$('formError').hidden=true;editingId=id;editorMode=id===null?'create':'topup';
  const u=accounts.find(a=>a.id===id),topup=editorMode==='topup';if(topup&&!u)return;
  $('editorTitle').textContent=topup?'Isi ulang paket':'Buat akun';$('editorSubtitle').textContent=topup?`Tambahkan token atau hari untuk ${u.name}.`:'Paket yang pas untuk kebutuhan user.';
  $('nameField').hidden=topup;$('accountName').disabled=topup;$('accountName').required=!topup;$('planField').hidden=topup;$('formSteps').hidden=topup;$('planType').value=topup?u.plan:'token';$('daysInput').value=topup?'':'7';$('daysInput').required=!topup;
  $('submitAccount').innerHTML=icon('plus')+(topup?'Tambahkan ke paket':'Buat akun & API key');syncForm();$('editorDialog').showModal();
}
function openDetail(id,created=false){
  const u=accounts.find(a=>a.id===id);if(!u)return;
  $('detailContent').innerHTML=`<header class="modal-heading"><div><p class="eyebrow">${created?'AKUN BARU':'DETAIL AKUN'}</p><h2 id="detailTitle">${created?'Akun berhasil dibuat.':'Kelola akun'}</h2></div><button class="icon-button" data-action="close-detail" aria-label="Tutup detail">${icon('x')}</button></header><div class="modal-body">${created?`<div class="success-heading">${icon('check-circle')}Paket dan API key sudah siap.</div>`:''}<div class="detail-user">${avatar(u)}<div><strong>${esc(u.name)}</strong><span>${u.plan==='token'?'Kuota token':'Unlimited'} · ${remaining(u)}</span></div><span style="margin-left:auto">${badge(u)}</span></div><div class="detail-grid"><div class="detail-stat"><span>${u.plan==='token'?'Sisa kuota':'Token terpakai'}</span><strong>${u.plan==='token'?compact(Math.max(0,u.limit-u.used)):compact(u.input+u.output)}</strong></div><div class="detail-stat"><span>Berakhir · WIB</span><strong style="font-size:15px">${date(u.expires)}</strong></div><div class="detail-stat"><span>Token input</span><strong>${compact(u.input)}</strong></div><div class="detail-stat"><span>Token output</span><strong>${compact(u.output)}</strong></div></div><label class="field">API key<div class="copy-field"><code class="key-field" id="detailKey">${esc(maskedKey(u))}</code><button class="icon-button" data-action="reveal" data-id="${u.id}" aria-label="Tampilkan API key">${icon('key')}</button><button class="icon-button" data-action="copy-key" data-id="${u.id}" aria-label="Salin API key">${icon('copy')}</button></div><small>Bagikan key hanya kepada pemilik akun.</small></label><label class="field">API base<div class="copy-field"><code>${esc(u.apiBase)}</code></div></label><div class="detail-actions"><button class="button primary" data-action="topup" data-id="${u.id}">${icon('plus')}Tambah token / hari</button><button class="button secondary" data-action="extend" data-id="${u.id}">${icon('clock')}+24 jam</button><button class="button secondary" data-action="copy-url" data-id="${u.id}">${icon('copy')}Salin tautan user</button>${u.review?`<button class="button secondary" data-action="review" data-id="${u.id}">${icon('shield')}Review usage</button>`:''}</div></div><footer class="modal-footer"><button class="button secondary danger-outline" data-action="delete" data-id="${u.id}">Hapus akun</button><button class="button secondary" data-action="toggle-revoke" data-id="${u.id}">${u.revoked?'Pulihkan akses':'Cabut akses'}</button><button class="button secondary" data-action="close-detail">Tutup</button></footer>`;
  $('detailDialog').showModal();
}
$('accountForm').addEventListener('submit',async ev=>{
  ev.preventDefault();if(mutationBusy)return;$('formError').hidden=true;
  try{
    const plan=$('planType').value,topup=editorMode==='topup',tokens=plan==='token'?tokensFromM($('tokenInput').value,topup):0;
    const days=$('daysInput').value.trim()===''?0:Number($('daysInput').value);
    if(!Number.isInteger(days)||days<0||days>365||(!topup&&days<1))throw new Error('Masa aktif harus berupa hari bulat antara 1–365.');
    if(topup&&tokens===0&&days===0)throw new Error('Tambahkan token, hari, atau keduanya.');
    const name=$('accountName').value.trim();if(!topup&&!name)throw new Error('Isi nama akun terlebih dahulu.');
    $('submitAccount').disabled=true;
    const result=topup?await mutate(`/api/admin/users/${editingId}/topup`,{token_limit:tokens,days}):await mutate('/api/admin/users',{name,plan_type:plan,days,...(plan==='token'?{token_limit:tokens}:{})});
    upsert(result);$('editorDialog').close();
    if(topup){notify('Paket berhasil diisi ulang.');await refreshAfterMutation();}
    else{activeFilter='all';$('searchInput').value='';$('planFilter').value='all';page=1;document.querySelectorAll('[data-filter]').forEach(b=>b.setAttribute('aria-selected',b.dataset.filter==='all'?'true':'false'));location.hash='akun';render();openDetail(result.id,true);await refreshAfterMutation();}
  }catch(error){$('formError').textContent=error.message;$('formError').hidden=false;}finally{$('submitAccount').disabled=false;}
});
['planType','tokenInput','daysInput'].forEach(id=>$(id).addEventListener(id==='planType'?'change':'input',syncForm));
function openReview(id){
  const u=accounts.find(a=>a.id===id);if(!u||!u.review)return;reviewingId=id;$('usageForm').reset();
  const q=u.issue||{};$('usageUser').textContent=`${u.name} · ${q.model||'Model tidak tersedia'} · laporan parsial ${nf.format(q.partial_input_tokens||0)} input / ${nf.format(q.partial_output_tokens||0)} output.`;
  $('usageError').hidden=true;syncUsage();$('usageDialog').showModal();
}
function syncUsage(){const bill=$('usageMode').value==='bill';$('actualTokens').hidden=!bill;['actualInput','actualOutput'].forEach(id=>{$(id).disabled=!bill;$(id).required=bill;});}
$('usageMode').addEventListener('change',syncUsage);
$('usageForm').addEventListener('submit',async ev=>{
  ev.preventDefault();if(mutationBusy)return;const u=accounts.find(a=>a.id===reviewingId);if(!u)return;
  const button=$('usageForm').querySelector('[type="submit"]');$('usageError').hidden=true;
  try{
    const bill=$('usageMode').value==='bill',reason=$('usageReason').value.trim();if(!reason)throw new Error('Isi catatan atau alasan penyelesaian.');
    const body={mode:bill?'bill':'waive',reason};
    if(bill){const input=Number($('actualInput').value),output=Number($('actualOutput').value);if(!Number.isSafeInteger(input)||!Number.isSafeInteger(output)||input<0||output<0||!Number.isSafeInteger(input+output))throw new Error('Masukkan laporan token aktual yang valid.');Object.assign(body,{input_tokens:input,output_tokens:output});}
    button.disabled=true;await mutate(`/api/admin/quota-requests/${encodeURIComponent(u.pending)}/resolve`,body);$('usageDialog').close();notify('Usage sudah diselesaikan.');await refreshAfterMutation();
  }catch(error){$('usageError').textContent=error.message;$('usageError').hidden=false;}finally{button.disabled=false;}
});
document.addEventListener('click',async ev=>{
  const button=ev.target.closest('[data-action]');if(!button)return;const action=button.dataset.action,id=Number(button.dataset.id),u=accounts.find(a=>a.id===id);
  try{
    if(action==='create')openEditor();else if(action==='detail')openDetail(id);
    else if(action==='close-editor'){if(!mutationBusy)$('editorDialog').close();}
    else if(action==='close-detail')$('detailDialog').close();else if(action==='close-usage'){if(!mutationBusy)$('usageDialog').close();}
    else if(action==='topup'){$('detailDialog').close();openEditor(id);}
    else if(action==='review'){$('detailDialog').close();openReview(id);}
    else if(action==='reveal'&&u){$('detailKey').textContent=u.key;button.disabled=true;}
    else if(action==='copy-key'&&u)await copyText(u.key);else if(action==='copy-url'&&u)await copyText(u.url);else if(action==='copy-base')await copyText(apiBase);
    else if(action==='extend'&&u){button.disabled=true;const result=await mutate(`/api/admin/users/${id}/extend`,{hours:24});upsert(result);$('detailDialog').close();notify('Masa aktif ditambah 24 jam.');await refreshAfterMutation();}
    else if(action==='toggle-revoke'&&u){button.disabled=true;const result=await mutate(`/api/admin/users/${id}/${u.revoked?'restore':'revoke'}`);upsert(result);$('detailDialog').close();notify(u.revoked?'Akses akun dipulihkan.':'Akses akun dicabut.');await refreshAfterMutation();}
    else if(action==='delete'&&u){if(!confirm(`Hapus akun ${u.name}? API key dan riwayat akun ini akan dihapus. Tindakan tidak dapat dibatalkan.`))return;button.disabled=true;await mutate(`/api/admin/users/${id}`,undefined,'DELETE');accounts=accounts.filter(a=>a.id!==id);$('detailDialog').close();render();notify('Akun dihapus.');await refreshAfterMutation();}
  }catch(error){notify(error.message);button.disabled=false;}
});
// Escape never closes a form while its request is still being committed.
[$('editorDialog'),$('usageDialog')].forEach(dialog=>dialog.addEventListener('cancel',ev=>{if(mutationBusy)ev.preventDefault();}));
document.querySelectorAll('[data-filter]').forEach(button=>button.addEventListener('click',()=>{activeFilter=button.dataset.filter;page=1;document.querySelectorAll('[data-filter]').forEach(b=>b.setAttribute('aria-selected',b===button?'true':'false'));renderAccounts();}));
['searchInput','planFilter'].forEach(id=>$(id).addEventListener(id==='searchInput'?'input':'change',()=>{page=1;renderAccounts();}));
$('prevPage').addEventListener('click',()=>{page--;renderAccounts();});$('nextPage').addEventListener('click',()=>{page++;renderAccounts();});
$('reviewLink').addEventListener('click',()=>location.hash='review');$('notificationButton').addEventListener('click',()=>location.hash='review');
$('menuButton').addEventListener('click',()=>{const open=$('sidebar').classList.toggle('open');$('menuButton').setAttribute('aria-expanded',String(open));});
document.addEventListener('click',ev=>{if($('sidebar').classList.contains('open')&&!ev.target.closest('#sidebar, #menuButton')){$('sidebar').classList.remove('open');$('menuButton').setAttribute('aria-expanded','false');}});
$('profileButton').addEventListener('click',()=>notify('Administrator · Arbyte workspace.'));
$('refreshButton').addEventListener('click',async()=>{if(await load())notify('Data portal diperbarui.');});$('retryLoad').addEventListener('click',()=>load());
$('exportButton').addEventListener('click',()=>{
  if(!loaded)return;
  const rows=[['Nama','Paket','Batas token','Kuota terpakai','Input','Output','Request','Status','Berakhir WIB'],...accounts.map(u=>[u.name,u.plan,u.limit,u.used,u.input,u.output,u.requests,statuses[status(u)],date(u.expires)])];
  const csv='\uFEFF'+rows.map(row=>row.map(value=>{let text=String(value);if(/^[=+\-@\t\r\n]/.test(text))text="'"+text;return '"'+text.replace(/"/g,'""')+'"';}).join(',')).join('\r\n');
  const url=URL.createObjectURL(new Blob([csv],{type:'text/csv;charset=utf-8'})),a=document.createElement('a');a.href=url;a.download='arbyte-akun.csv';a.click();setTimeout(()=>URL.revokeObjectURL(url),1000);notify('Ringkasan akun diekspor.');
});
function curlExample(){return apiBase?`curl -N ${apiBase}/chat/completions \\
  -H "Authorization: Bearer $ARBYTE_API_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "deepseek-v4.1-flash-free",
    "stream": true,
    "messages": [
      {"role":"system","content":"You are a helpful assistant."},
      {"role":"user","content":"Hello!"}
    ]
  }'`:'Memuat konfigurasi API…';}
$('copyCurl').addEventListener('click',()=>{if(apiBase)copyText(curlExample());});
window.addEventListener('hashchange',navigate);$('year').textContent=new Date(currentTime()).getFullYear();hydrateIcons();render();renderChart();navigate();load();
setInterval(()=>{if(!document.hidden&&!document.querySelector('dialog[open]')&&!mutationBusy)load();},30000);
document.addEventListener('visibilitychange',()=>{if(!document.hidden&&!document.querySelector('dialog[open]')&&!mutationBusy)load();});
