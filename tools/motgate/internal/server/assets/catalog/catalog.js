(() => {
  'use strict';
  const $ = s => document.querySelector(s);
  const modelsEl = $('#models'), packagesEl = $('#packages');
  let catalog;
  let routerModels = [], routerStatus = 'Memuat katalog router…';
  const controls = ['#save', '#add-model', '#add-package'].map($);
  controls.forEach(b => b.disabled = true);

  function notice(message, bad = false) {
    const n = $('#notice');
    n.textContent = message;
    n.hidden = false;
    n.classList.toggle('error', bad);
    n.scrollIntoView({behavior: 'smooth', block: 'center'});
  }

  function modelChoices(card, pack) {
    const scope = card.querySelector('[data-field="model_scope"]');
    const group = card.querySelector('.model-options');
    const choices = card.querySelector('.model-choices');
    const selected = pack.model_scope === 'selected';
    scope.value = selected ? 'selected' : 'all';
    group.hidden = !selected;
    choices.replaceChildren();
    const help = card.querySelector('.model-help');
    help.textContent = selected
      ? 'Model yang dicentang akan tercantum pada kartu harga. Ini tidak mengubah izin API akun.'
      : `${catalog.models.filter(m => m.enabled).length} model aktif akan tampil otomatis. Model baru ikut ditampilkan. Ini tidak mengubah izin API akun.`;
    const query = card.querySelector('.model-search').value.trim().toLowerCase();
    card.querySelector('.router-status').textContent = routerStatus;
    const available = new Map(catalog.models.map(m => [m.id.toLowerCase(), m]));
    for (const m of routerModels) if (!available.has(m.id.toLowerCase())) available.set(m.id.toLowerCase(), {...m, fromRouter:true});
    let visible = 0;
    for (const model of available.values()) {
      if (query && ![model.id, model.name, model.provider || ''].join(' ').toLowerCase().includes(query)) continue;
      visible++;
      const label = document.createElement('label');
      label.className = 'model-choice';
      const input = document.createElement('input');
      input.type = 'checkbox';
      input.checked = (pack.models || []).includes(model.id);
      const text = document.createElement('span');
      const name = document.createElement('strong');
      name.textContent = (model.name || model.id || 'Model baru') + (model.fromRouter ? ' · dari router' : model.enabled ? '' : ' · disembunyikan');
      const id = document.createElement('small');
      id.textContent = model.id + (model.provider ? ` · ${model.provider}` : '');
      text.append(name, id);
      label.append(input, text);
      input.addEventListener('change', () => {
        if (input.checked && !catalog.models.some(m => m.id.toLowerCase() === model.id.toLowerCase())) {
          if (catalog.models.length >= 200) { input.checked = false; notice('Batas katalog 200 model.', true); return; }
          catalog.models.push({id:model.id, name:model.name || model.id, description:'Model katalog ' + (model.provider || 'router') + '. Ketersediaan mengikuti provider.', icon:'AI', color:'#caff42', enabled:true});
          drawModels();
        }
        const ids = new Set(pack.models || []);
        input.checked ? ids.add(model.id) : ids.delete(model.id);
        pack.models = [...ids];
      });
      choices.append(label);
    }
    if (!visible) choices.textContent = 'Tidak ada model yang cocok. Kamu juga bisa menambahkan Model ID secara manual.';
  }

  function refreshChoices() {
    packagesEl.querySelectorAll('.package').forEach((card, i) => modelChoices(card, catalog.packages[i]));
  }

  function bindCard(card, obj, kind) {
    card.querySelector('.card-title strong').textContent = obj.name || (kind === 'models' ? 'Model baru' : 'Paket baru');
    for (const input of card.querySelectorAll('[data-field]')) {
      const key = input.dataset.field;
      if (key === 'features') input.value = (obj[key] || []).join('\n');
      else if (input.type === 'checkbox') input.checked = !!obj[key];
      else input.value = obj[key] || (key === 'model_scope' ? 'all' : '');
      input.addEventListener(input.type === 'checkbox' || input.tagName === 'SELECT' ? 'change' : 'input', () => {
        const old = obj[key];
        obj[key] = key === 'features' ? input.value.split('\n').map(x => x.trim()).filter(Boolean)
          : input.type === 'checkbox' ? input.checked : input.value;
        if (key === 'name') card.querySelector('.card-title strong').textContent = obj.name || (kind === 'models' ? 'Model baru' : 'Paket baru');
        if (kind === 'models' && ['id', 'name', 'enabled'].includes(key)) {
          if (key === 'id') for (const pack of catalog.packages) pack.models = (pack.models || []).map(id => id === old ? obj.id : id);
          refreshChoices();
        }
        if (key === 'model_scope') {
          if (obj.model_scope === 'all') obj.models = [];
          modelChoices(card, obj);
        }
      });
    }
    card.querySelector('.remove').onclick = () => {
      if (kind === 'models') for (const p of catalog.packages) p.models = (p.models || []).filter(id => id !== obj.id);
      catalog[kind].splice(catalog[kind].indexOf(obj), 1);
      draw();
    };
    if (kind === 'packages') {
      card.querySelector('.model-search').addEventListener('input', () => modelChoices(card, obj));
      card.querySelector('.refresh-router').onclick = loadRouterModels;
      modelChoices(card, obj);
    }
    return card;
  }

  function drawModels() {
    modelsEl.replaceChildren();
    for (const m of catalog.models) modelsEl.append(bindCard($('#model-template').content.firstElementChild.cloneNode(true), m, 'models'));
  }
  function draw() {
    drawModels(); packagesEl.replaceChildren();
    for (const p of catalog.packages) packagesEl.append(bindCard($('#package-template').content.firstElementChild.cloneNode(true), p, 'packages'));
  }

  $('#add-model').onclick = () => {
    catalog.models.push({id:'', name:'', description:'', icon:'A', color:'#caff42', enabled:true});
    draw(); modelsEl.lastElementChild.querySelector('[data-field="name"]').focus();
  };
  $('#add-package').onclick = () => {
    catalog.packages.push({name:'', label:'', price:'', description:'', features:[], model_scope:'all', models:[], button:'Hubungi admin', url:'https://wa.me/', enabled:true});
    draw(); packagesEl.lastElementChild.querySelector('[data-field="name"]').focus();
  };
  $('#save').onclick = async () => {
    for (const input of document.querySelectorAll('.card input, .card textarea')) {
      if (!input.checkValidity()) { input.reportValidity(); return; }
    }
    const button = $('#save'); button.disabled = true; button.textContent = 'Menyimpan…';
    try {
      const r = await fetch('/api/admin/catalog', {method:'PUT', headers:{'Content-Type':'application/json'}, body:JSON.stringify(catalog)});
      const data = await r.json().catch(() => ({}));
      if (!r.ok) throw Error(data.error?.message || 'Gagal menyimpan (' + r.status + ')');
      notice('Tersimpan. Daftar model dan kartu paket di landing sudah diperbarui.');
    } catch (e) { notice(e.message, true); }
    finally { button.disabled = false; button.textContent = 'Simpan perubahan'; }
  };
  async function loadRouterModels() {
    packagesEl.querySelectorAll('.refresh-router').forEach(b => b.disabled = true);
    routerStatus = 'Memuat katalog router…'; refreshChoices();
    try {
      const response = await fetch('/api/admin/catalog/router-models', {cache:'no-store'});
      const data = await response.json();
      if (!response.ok) throw Error(data.error?.message || 'Katalog router tidak tersedia.');
      routerModels = data.models;
      routerStatus = `${routerModels.length} model dari router. Pilihan baru akan ditambahkan ke landing setelah disimpan. Daftar ini bukan verifikasi akses atau harga provider.`;
    } catch (e) { routerStatus = e.message + ' Model tersimpan tetap bisa dipilih.'; }
    finally { refreshChoices(); packagesEl.querySelectorAll('.refresh-router').forEach(b => b.disabled = false); }
  }
  fetch('/api/admin/catalog', {cache:'no-store'}).then(async r => {
    if (!r.ok) throw Error('Tidak dapat memuat katalog (' + r.status + ')');
    catalog = await r.json(); draw(); controls.forEach(b => b.disabled = false); loadRouterModels();
  }).catch(e => notice(e.message, true));
})();
