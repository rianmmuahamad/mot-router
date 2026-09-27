# Deployment VPS

Mot Router jalan 24 jam di VPS `mot` (43.159.58.17, Singapore, Ubuntu 20.04).

---

## 1. Akses

Dashboard: **https://mot.coddx.store**

| | |
|---|---|
| Username | `rian` |
| Password | lihat `/tmp/mot_creds.txt` di laptop, atau `docs/VPS-CREDENTIALS.txt` |

Basic auth dipasang karena dashboard Mot Router **tidak punya login sendiri**.
Tanpa proteksi, siapa pun yang tahu alamatnya bisa membaca API key, melihat log
request, dan memakai kuota model gratis.

> **DNS belum dibuat.** `mot.coddx.store` belum punya A record, jadi HTTPS belum
> bisa diterbitkan Let's Encrypt. Sampai record dibuat, akses lewat SSH tunnel.

### Sementara: akses lewat SSH tunnel

```bash
ssh -L 8787:127.0.0.1:8787 mot
# lalu buka http://127.0.0.1:8787 di browser
```

### Yang perlu Anda lakukan agar HTTPS aktif

Di dashboard Cloudflare untuk `coddx.store`, tambahkan:

| Tipe | Nama | Isi | Proxy |
|---|---|---|---|
| A | `mot` | `43.159.58.17` | **DNS only (abu-abu)** dulu |

Proxy harus **mati dulu** supaya Let's Encrypt bisa memverifikasi lewat HTTP.
Setelah Caddy berhasil menerbitkan sertifikat (±1 menit), proxy boleh
dinyalakan lagi — Caddy akan memperbarui sendiri.

Caddy sudah dikonfigurasi dan otomatis mencoba ulang setiap 60 detik, jadi
begitu record dibuat, HTTPS langsung aktif tanpa tindakan tambahan.

---

## 2. Arsitektur di VPS

```
                    Internet
                       │
                       ▼
              ┌──────────────────┐
              │  Caddy :443/:80  │  TLS + basic auth
              │  mot.coddx.store │
              └────────┬─────────┘
                       │ reverse_proxy
                       ▼
              ┌──────────────────┐
              │ mot-router :8787 │  bind 127.0.0.1 (tidak publik)
              │  systemd service │
              └────────┬─────────┘
                       │ HTTP proxy
                       ▼
              ┌──────────────────┐
              │  mihomo :7891    │  group MOT
              │  + control :9091 │
              └────────┬─────────┘
                       │
            ┌──────────┴──────────┐
            ▼                     ▼
     ┌────────────┐        ┌────────────┐
     │  DIRECT    │        │   WARP     │
     │ IP VPS     │        │ 127.0.0.1  │
     │ 43.159..17 │        │   :40000   │
     └────────────┘        └────────────┘
        jalur utama          cadangan
```

Router **hanya listen di `127.0.0.1`**, jadi port 8787 tidak bisa diakses
langsung dari internet. Sudah diverifikasi: `curl http://43.159.58.17:8787`
dari luar gagal.

---

## 3. Kenapa DIRECT jadi jalur utama

Diukur dari VPS, 8 request per jalur:

| Jalur | Hasil | Egress IP |
|---|---|---|
| **DIRECT** (IP VPS) | **8/8 berhasil**, 1,4–7,8 detik | 43.159.58.17 |
| WARP | **429 konsisten** | 104.28.222.43 |

Ini **kebalikan** dari situasi di laptop, di mana WARP yang bekerja dan IP
langsung kena limit. Penyebabnya: pool IP WARP kecil dan dipakai bersama semua
pengguna gratis, jadi tingkat kejenuhannya berbeda per lokasi.

Karena itu:

- Group `MOT` menaruh `DIRECT` di urutan pertama.
- **Rotasi otomatis dimatikan** (`rotate_on_limit: false`). Group ini hanya punya
  dua pilihan dan salah satunya jenuh, jadi rotasi justru bisa memindahkan
  request dari jalur yang bekerja ke jalur yang gagal.

> Retry **tetap berjalan** walau rotasi mati. Kegagalan sesaat seperti koneksi
> putus dicoba ulang di node yang sama; hanya kegagalan yang melekat pada IP
> (rate limit, blokir region) yang langsung berhenti.

WARP tetap terpasang sebagai cadangan. Kalau suatu saat IP VPS kena limit,
pindah node lewat dashboard atau:

```bash
curl -X POST http://127.0.0.1:8787/api/mihomo/select \
  -H 'Content-Type: application/json' \
  -d '{"group":"MOT","node":"WARP"}'
```

---

## 4. Layanan

Semua `enabled` + `active`, sudah diuji tahan reboot.

| Service | Fungsi | Config |
|---|---|---|
| `warp-svc` | Cloudflare WARP, proxy di `:40000` | bawaan paket |
| `mihomo-mot` | mihomo group `MOT` | `/etc/mihomo-mot/config.yaml` |
| `mot-router` | router + dashboard | `/opt/mot-router/` |
| `caddy` | TLS + basic auth + reverse proxy | `/etc/caddy/Caddyfile` |

### Perintah harian

```bash
# Status semua
sudo systemctl status mot-router mihomo-mot warp-svc caddy --no-pager

# Log router (60 baris terakhir)
sudo journalctl -u mot-router -n 60 --no-pager

# Ikuti log langsung
sudo journalctl -u mot-router -f

# Restart
sudo systemctl restart mot-router
```

### Lokasi berkas

| Path | Isi |
|---|---|
| `/opt/mot-router/mot-router` | binary |
| `/opt/mot-router/data/mot-router.db` | database (API key, log, setting) |
| `/opt/mot-router/README.md` | dokumentasi |
| `/opt/mot-router/docs/IP-ROTATION.md` | arsitektur rotasi |
| `/etc/mihomo-mot/config.yaml` | node DIRECT + WARP |
| `/etc/caddy/Caddyfile` | reverse proxy + basic auth |

Backup binary lama tersimpan sebagai `/opt/mot-router/mot-router.bak.*`.

---

## 5. API

Gunakan API key dari dashboard (tab **API Keys**).

```bash
KEY=mr_API_KEY_ANDA

curl https://mot.coddx.store/v1/chat/completions \
  -u 'rian:PASSWORD_DASHBOARD' \
  -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"opencode-zen/muse-spark-1.3-contributor-free",
       "messages":[{"role":"user","content":"Halo"}],
       "reasoning_effort":"high",
       "stream":false}'
```

Basic auth hanya untuk dashboard dan API manajemen. Endpoint `/v1/*` tetap
memerlukan API key Mot Router.

### Model tersedia

| Model | Reasoning | Catatan |
|---|---|---|
| `opencode-zen/muse-spark-1.3-contributor-free` | ya (`reasoning_effort`) | free tier, jalur utama |
| `space-bunny-free` | ya | Zen community, jalur `chat/completions` |
| `mimo-v2.6-flash-free` | ya | Zen, konteks 1M |
| `qwen3.8-flash:free` | ya | Token Harbor, campaign + pool kunci khusus Qwen |
| `deepseek-v4.1-flash:free` | ya | Token Harbor, rolling 7 hari, failover banyak kunci |
| `cline/glm-5.2` | ya | Cline (Z.ai GLM 5.2), alias `glm-5.2` / `zai/glm-5.2` |
| `cline-free/gemini-3.8-flash` | ya | Cline free tier |
| `cline-free/deepseek-v4.1-flash` | ya | Cline free tier |

Penalaran muncul sebagai field `reasoning_content` (OpenAI) atau blok `thinking`
(Anthropic). Tanpa `reasoning_effort`, model tidak mengirim penalaran — itu
perilaku upstream, bukan router yang membuangnya.

---

## 6. Hasil verifikasi

Diukur setelah deploy dan reboot:

```
Inference      5/5 berhasil, 1,6–6,6 detik
Gate test      3/3 ok, 2,0–4,1 detik, balasan GATE_OK
Profil gate    verdict: ok, semua 7 kondisi lolos
Tool wajib     bash, edit, glob, grep, read
Reasoning      reasoning_content terisi
Reboot         semua 4 service pulih otomatis
```

Egress terverifikasi `43.159.58.17` lewat mihomo, dan port 8787 tidak terekspos.

---

## 7. Masalah yang ditemukan saat deploy

Deploy ke VPS dengan rute jaringan yang lebih flaky memunculkan dua bug yang
tidak terlihat di laptop:

### 7.1 Hasil tidak meyakinkan dianggap penolakan

Prober gate mencari tool minimum dengan membuang nama satu per satu. Sebuah
koneksi putus tidak menghasilkan penerimaan **maupun** penolakan, tapi dibaca
sebagai penolakan — sehingga nama tool itu dianggap wajib.

Terukur: daftar tool wajib menggelembung dari **5 nama jadi 8**.

Perbaikan: `inconclusive()` sekarang mencakup kegagalan transport, dan
pembuangan nama diulang maksimal 3 kali. Hanya penolakan pasti yang dianggap
bukti.

### 7.2 Prober memindahkan node walau rotasi dimatikan

`prepareGate()` selalu memasang hook rotasi tanpa memeriksa toggle. Akibatnya
probe bisa memindahkan egress yang dipakai request nyata.

Terukur: node berpindah dari `DIRECT` (yang bekerja) ke `WARP` (yang kena 429)
saat probe berjalan.

Perbaikan: `prepareGate()` menghormati `rotate_on_limit`. Probe harus
**mengamati** egress yang dikonfigurasi, bukan mengubahnya.

---

## 8. Catatan

- **Langganan proxy masih habis.** Node berbayar (`苏菲家宽`) tidak dipakai di
  VPS. Kalau Anda memperpanjang, tambahkan sebagai provider mihomo lalu pilih
  group-nya.
- **IP VPS bisa kena limit.** Ini satu IP tanpa rotasi. Kalau terjadi, pindah ke
  WARP sementara atau tambahkan langganan.
- **Basic auth melindungi dashboard**, tapi kredensial dikirim tiap request.
  HTTPS wajib aktif sebelum dipakai dari jaringan publik — pastikan DNS sudah
  dibuat.
- **WARP di VPS jenuh.** Jangan jadikan jalur utama; sudah diukur 429 konsisten.
- **Backup config Caddy** ada di `/etc/caddy/Caddyfile.bak.*`.

---

## 9. Error 525 dan konflik header Authorization

Dua masalah yang muncul setelah DNS record dibuat.

### 9.1 Error 525: Cloudflare ↔ origin handshake gagal

Gejala: browser menampilkan `SSL handshake failed Error code 525` dari Cloudflare.

**Penyebabnya lingkaran setan:**

1. Cloudflare memproksi `:443` ke origin, jadi Cloudflare harus melakukan TLS
   handshake ke Caddy.
2. Caddy belum punya sertifikat untuk `mot.coddx.store`, jadi handshake ditolak.
3. Let's Encrypt tidak bisa menerbitkan sertifikat karena:
   - **HTTP-01** — Cloudflare me-redirect `308` ke HTTPS, sementara ACME
     memerlukan respons langsung di `http://` pada path challenge.
   - **TLS-ALPN-01** — Cloudflare memutus TLS di edge dan tidak meneruskan
     negosiasi ALPN ke origin (`Cannot negotiate ALPN protocol "acme-tls/1"`).

**Perbaikan:** pakai `tls internal` untuk blok itu. Sertifikat internal membuat
handshake Cloudflare → origin berhasil, sehingga 525 hilang. Pengunjung tetap
melihat sertifikat edge Cloudflare (`Google Trust Services`), jadi tidak ada
peringatan di browser.

```caddyfile
mot.coddx.store {
    tls internal
    ...
}
```

Alternatif yang lebih bersih kalau Anda punya API token Cloudflare: pakai
**DNS-01 challenge** (`caddy-dns/cloudflare`), yang tidak butuh port 80/443
sama sekali. Belum dipakai karena token tidak tersedia.

> Kalau nanti Anda mau sertifikat Let's Encrypt asli di origin, matikan
> **Proxy** (abu-abu) sementara, tunggu Caddy menerbitkan sertifikat, lalu
> nyalakan lagi. Setelah itu `tls internal` bisa dihapus.

### 9.2 Konflik header `Authorization`

Setelah 525 hilang, inference lewat domain mengembalikan **401** meski API key
benar.

**Penyebabnya:** basic auth Caddy dan API key Mot Router sama-sama memakai
header `Authorization`. Klien tidak bisa mengirim dua nilai sekaligus:

```
curl -u user:pass -H "Authorization: Bearer <key>" ...
  -> basic auth menang, endpoint inference melihat kredensial yang salah
  -> HTTP 401
```

Terukur:

| Permintaan | Hasil |
|---|---|
| hanya basic auth | 200 (tapi endpoint inference tidak dapat API key) |
| hanya Bearer | gagal di basic auth |
| keduanya | **401** — konflik |

**Perbaikan:** `/v1/*` dan `/oauth/*` dikecualikan dari basic auth. Endpoint itu
sudah punya proteksi sendiri (API key, terbukti `401` tanpa key), jadi satu
lapis sudah cukup. Dashboard dan API manajemen tetap di balik basic auth.

```caddyfile
mot.coddx.store {
    tls internal

    @inference path /v1/* /oauth/*
    handle @inference {
        reverse_proxy 127.0.0.1:8787
    }

    handle {
        basic_auth {
            rian <hash>
        }
        reverse_proxy 127.0.0.1:8787
    }
}
```

Hasil verifikasi:

| Uji | Hasil |
|---|---|
| inference dengan Bearer saja | **200**, balasan `DOMAIN_OK` |
| inference tanpa API key | 401 |
| dashboard tanpa basic auth | 401 |
| dashboard dengan basic auth | 200 |
| `/api/keys` tanpa basic auth | 401 |
| `whitesecurity.id` | 200 (tidak terpengaruh) |

### Cara pakai API sekarang

Karena `/v1/*` tidak lagi butuh basic auth, klien cukup mengirim API key:

```bash
curl https://mot.coddx.store/v1/chat/completions \
  -H "Authorization: Bearer $MOT_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"opencode-zen/muse-spark-1.3-contributor-free",
       "messages":[{"role":"user","content":"Halo"}]}'
```

Ini juga membuat router bisa dipakai langsung sebagai provider OpenAI-compatible
di klien mana pun, tanpa konfigurasi basic auth tambahan.

---

## 10. Rotasi tidak berfungsi: DIRECT disaring dari daftar node

Gejala: 12 request berturut-turut semuanya `429 FreeUsageLimitError`, meski
pengujian langsung menunjukkan IP VPS berhasil 6/6 dan hanya WARP yang kena
limit.

**Penyebabnya satu baris di `leafNodes()`.** Fungsi itu menyaring `DIRECT`
dengan alasan "bukan server upstream":

```go
if name == "DIRECT" || name == "REJECT" {
    continue
}
```

Untuk langganan berbayar, asumsi itu benar — DIRECT memang bukan node. Tapi di
deployment VPS, group `MOT` sengaja berisi DIRECT karena **IP VPS justru jalur
yang bekerja**, sedangkan WARP kena limit.

Akibatnya group dua-node itu menyusut jadi **satu** node. Rotasi berputar di
node yang sama. Log membuktikannya:

```
mihomo rotate: from="WARP" to="WARP"
```

Rotasi "berhasil" secara teknis (tidak ada error) tapi tidak memindahkan apa pun.

**Perbaikan:** `DIRECT` dipertahankan sebagai exit yang sah. `REJECT` tetap
dibuang. DIRECT juga tidak punya entri health di beberapa build mihomo, jadi
diterima tanpa data delay.

Sekarang log menunjukkan perpindahan yang benar:

```
mihomo retry: attempt=1 status=429 ipTied=true node="WARP"
mihomo rotate: from="WARP" to="DIRECT"
```

### Penyingkiran node yang kena limit

Bug di atas memperlihatkan masalah lebih dalam: **mihomo menganggap node sehat
hanya karena tunnelnya jalan.** Ia tidak tahu Zen menolak exit tertentu. Jadi
node bisa "sehat" tapi tidak berguna.

Sekarang router mencatat node yang gagal dengan cara yang melekat pada IP
(rate limit, blokir region) selama 10 menit, lalu rotasi menghindarinya:

```go
if ipTiedFailure(lastStatus, lastErr.Error()) {
    s.penalizeNode(group, cur)
}
```

Penyingkiran hanya berlaku kalau ada node lain — kalau tidak, akan tidak ada
tempat tujuan. Node yang berhasil menghapus catatannya, jadi pemulihan tidak
butuh restart.

### Hasil verifikasi

| Skenario | Hasil |
|---|---|
| rotasi menyala, 8 request | **8/8 berhasil**, rata-rata ~3 detik |
| dipaksa ke WARP, 1 request | **200**, otomatis pindah ke DIRECT |
| node akhir | DIRECT, egress `43.159.58.17` |
| inference lewat domain publik | **200**, balasan `FIXED` |
| dashboard dengan auth | 200 |
| dashboard tanpa auth | 401 |
