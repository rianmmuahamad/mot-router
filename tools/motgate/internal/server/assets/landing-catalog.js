(() => {
  'use strict';
  const grid = document.querySelector('.model-grid'), pricing = document.querySelector('.price-layout');
  if (!grid || !pricing) return;
  function el(tag, cls, text) {
    const n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text !== undefined) n.textContent = String(text);
    return n;
  }
  function paintModels(items) {
    grid.replaceChildren();
    items.forEach((m, i) => {
      const b = el('button', 'model-card');
      b.type = 'button'; b.setAttribute('aria-pressed', i === 0 ? 'true' : 'false');
      b.dataset.model = m.id; b.style.setProperty('--card-color', m.color);
      const top = el('span', 'model-top');
      top.append(el('span', 'model-icon', m.icon), el('span', 'model-check', '✓'));
      b.append(top, el('h3', '', m.name), el('p', '', m.description), el('span', 'model-id mono', m.id));
      b.addEventListener('click', () => {
        grid.querySelectorAll('.model-card').forEach(x => x.setAttribute('aria-pressed', x === b ? 'true' : 'false'));
        if (window.setCatalogModel) window.setCatalogModel(m.id);
      });
      grid.append(b);
    });
    if (items[0] && window.setCatalogModels) window.setCatalogModels(items);
  }
  function paintPackages(items, models) {
    pricing.replaceChildren();
    for (const p of items) {
      const card = el('article', 'offer-card');
      const header = el('header', 'offer-heading');
      header.append(el('span', 'offer-label', p.label), el('h3', 'offer-name', p.name));
      const amount = el('div', 'offer-amount');
      amount.append(el('p', 'offer-price', p.price), el('p', 'offer-description', p.description));
      const benefits = el('ul', 'offer-features');
      benefits.setAttribute('aria-label', 'Fitur ' + p.name);
      (p.features || []).forEach(f => benefits.append(el('li', '', f)));
      const modelSection = el('section', 'offer-models');
      const selected = p.model_scope === 'selected'
        ? models.filter(m => (p.models || []).includes(m.id)) : models;
      const title = el('div', 'offer-models-heading');
      title.append(el('h4', '', 'Model tersedia'), el('span', 'offer-model-count', selected.length + ' model'));
      const list = el('ul', 'offer-model-list');
      for (const model of selected) {
        const item = el('li', 'offer-model');
        item.append(el('span', 'offer-model-name', model.name), el('span', 'offer-model-id', model.id));
        list.append(item);
      }
      modelSection.append(title, list);
      if (!selected.length) modelSection.append(el('p', 'offer-model-empty', 'Daftar model sedang diperbarui.'));
      const footer = el('div', 'offer-footer');
      const link = el('a', 'offer-button', p.button + ' ↗');
      link.href = p.url; link.target = '_blank'; link.rel = 'noopener';
      const accountLink = el('a', 'offer-account-link', 'Sudah punya API key? Buka dashboard');
      accountLink.href = '/u';
      footer.append(link, accountLink);
      card.append(header, amount, benefits, modelSection, footer);
      pricing.append(card);
    }
    if (!items.length) pricing.append(el('p', 'section-copy', 'Informasi paket segera diperbarui.'));
  }
  fetch('/api/catalog', {headers:{Accept:'application/json'}, cache:'no-store'})
    .then(r => { if (!r.ok) throw Error('catalog'); return r.json(); })
    .then(c => {
      const models = Array.isArray(c.models) ? c.models : [];
      if (Array.isArray(c.models)) {
        paintModels(models);
        const count = document.querySelector('.stats-grid .stat:nth-child(2) strong');
        if (count) count.textContent = models.length + ' model';
      }
      if (Array.isArray(c.packages)) {
        paintPackages(c.packages, models);
        const price = document.querySelector('.stats-grid .stat:nth-child(3) strong');
        if (price && c.packages[0]) price.textContent = c.packages[0].price;
      }
    }).catch(() => {});
})();
