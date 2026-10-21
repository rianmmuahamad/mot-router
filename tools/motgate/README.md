# motgate — portal user untuk mot-router

Router kedua yang berfungsi sebagai **endpoint user**: jembatan ke mot-router, dengan halaman
admin (buat user + key + masa berlaku sekaligus), halaman status user (token terpakai dan kapan
akses habis), dan endpoint inferensi per-user.

```
client -> Caddy :443 -> motgate :8790 -> guard :8791 -> mot-router :8787 -> mihomo :7890 -> Zen
                          │                                │
                     users + expiry                  master key (mr_)
                     + usage per user                + rotasi IP (ipfarm)
```

## Model akses

- Key user berawalan `mg_` (48 hex), dibuat motgate; key mot-router tidak pernah sampai ke user.
- Setiap key punya `expires_at`. User **unlimited token** selama masa berlaku aktif.
- Admin bisa menambah user tanpa batas; satu form = nama + durasi → user + key + URL jadi.
- Masa berlaku ditegakkan di motgate (`403 access expired`); mot-router tidak tahu soal expiry.
- Paket dapat berupa `unlimited` atau `token`. Paket token memakai satu kolam kuota total selama
  masa aktif; input dan output dihitung sama, dan kuota tidak reset harian.
- Request inferensi paket token dibatasi satu request aktif per akun. Token hanya ditagih dari
  laporan input/output akhir yang lengkap dan model/endpoint terverifikasi. Usage yang hilang
  menahan request berikutnya sampai admin menyelesaikan pemeriksaan.

## Halaman

| URL | Isi |
|---|---|
| `/admin/catalog` | edit katalog model, kartu harga, dan pilihan model per kartu (semua aktif atau model tertentu) |
| `/admin` | form unlimited/token, kuota dalam M, durasi hari, saldo kuota, top-up, review usage, revoke/restore/delete |
| `/u/{key}` | dashboard menyesuaikan paket: sisa kuota, input/output, riwayat, grafik, hitung mundur, API key dan contoh integrasi |

Auth admin: `Authorization: Bearer <password>` atau HTTP Basic (password sama). Route
`/admin*` dan `/api/admin*` wajib lolos; `/healthz`, `/u/*`, `/api/user/*`, `/v1/*` publik.

## API

```
GET    /api/admin/users                  daftar user (termasuk time_left_seconds, expired, url, api_base)
POST   /api/admin/users                  {"name":"u1","hours":24} -> 201 user+key
POST   /api/admin/users                  {"name":"u1","plan_type":"token","token_limit":50000000,"days":3}
POST   /api/admin/users/{id}/extend      {"hours":24}
POST   /api/admin/users/{id}/topup       {"token_limit":1000000,"days":7} (salah satu boleh 0)
POST   /api/admin/quota-requests/{requestId}/resolve  bill dengan input/output aktual, atau waive dengan alasan
POST   /api/admin/users/{id}/revoke
POST   /api/admin/users/{id}/restore
DELETE /api/admin/users/{id}
GET    /api/admin/overview               total/aktif/expired/revoked + request & token hari ini
GET    /api/user/{key}                   status satu user
GET    /api/user/{key}/usage             grafik 24h/7d + riwayat per akun (page, limit <= 200, model, search)
GET    /healthz
```

Inferensi (butuh key `mg_`): `POST /v1/chat/completions`, `POST /v1/messages`,
`POST /v1/systemone`, `POST /zen/v1/systemone`, `GET /v1/models`.

## Konfigurasi

| Flag | Default | Env |
|---|---|---|
| `--host` / `--port` | `127.0.0.1` / `8790` | `MOTGATE_HOST` / `MOTGATE_PORT` |
| `--data` | `./data-motgate` | `MOTGATE_DATA` |
| `--public-url` | `http://127.0.0.1:8790` | `MOTGATE_PUBLIC_URL` |
| `--upstream` | `http://127.0.0.1:8791` | `MOTGATE_UPSTREAM` |
| `--admin-password` | — | `MOTGATE_ADMIN_PASSWORD` (**wajib**, exit 2 bila kosong) |
| `--default-hours` | 24 | `MOTGATE_DEFAULT_HOURS` |

`--upstream http://127.0.0.1:8787` melewati guard; pakai hanya untuk diagnosis.

## Akuntansi token

Motgate mengenali laporan akhir OpenAI `prompt_tokens`/`completion_tokens` dan Anthropic
`input_tokens`/`output_tokens`; usage kumulatif memakai nilai tertinggi yang dilaporkan. Placeholder
nol di awal stream Anthropic tidak dianggap laporan lengkap. Translator Anthropic mengirim pasangan
input/output aktual pada event akhir. Paket token saat ini mengizinkan model OpenAI-compatible
`deepseek-v4.1-flash-free` dan `muse-spark-1.3-contributor-free`. Nama lama
`deepseek-v4.1-flash` (termasuk prefix `opencode-zen/`) dikenali sebagai varian gratis
ketika router tidak memiliki kredensial upstream berbayar. Model eksplisit yang tidak
dikenali diteruskan dengan namanya sendiri agar upstream memberi error yang benar,
tanpa diam-diam menggantinya dengan model default. Alias `codebuddy/deepseek-v4.1-flash`
diukur sebagai model aktual Muse Spark sesuai respons router; model lain dan surface Anthropic
belum ditawarkan sampai pelaporan tokennya terbukti.

Jika respons sukses berakhir tanpa pasangan token final, request disimpan sebagai belum
terverifikasi dan akun ditahan. Restart saat inferensi berlangsung juga menandai usage belum
terverifikasi. Admin dapat memasukkan hitungan aktual, atau membebaskan request dengan alasan
tercatat. Request yang tidak mencapai respons upstream tidak mengurangi kuota. Permintaan terakhir
yang sudah berjalan boleh melewati batas kuota; inferensi selanjutnya ditolak.

## Build

```bash
cd tools/motgate && /usr/local/go/bin/go build -o motgate .
```

## Deploy

```bash
# di VPS, sebagai root, dari checkout /opt/mot-router
GUARD_MASTER_KEY= ADMIN_PASSWORD= ./deploy/deploy-portal.sh
```

Skrip itu: membuat master key mot-router, membangun guard + motgate, memasang
`guard.service` + `motgate.service`, menyambungkan blok Caddy `portal.coddx.store`, lalu
mengecek kedua `/healthz`. DNS `portal` → A record VPS dengan proxy **mati (abu-abu)** dulu
sampai sertifikat terbit.

## Portal admin

`/admin` memakai layout dashboard dengan sidebar Ringkasan, Akun & paket,
Review usage, dan Integrasi API. Statistik, pencarian/filter akun, isi ulang,
cabut/pulihkan akses, serta penyelesaian usage memakai data dan API admin asli.
API key tersamarkan sampai admin memilih tampilkan atau salin. Ekspor CSV
memuat ringkasan akun tanpa kredensial dan mencegah formula spreadsheet dari nama akun.

`GET /api/admin/usage?period=7d` mengagregasikan input/output seluruh akun
dalam tujuh hari kalender, termasuk hari ini, dengan batas hari Asia/Jakarta.
Riwayat tanpa laporan token final ditandai belum terverifikasi dan tidak diberi
estimasi. Grafik membaca hasil agregasi dari SQLite; indeks timestamp membatasi
pembacaan ke periode yang diminta. Halaman/API admin memakai autentikasi yang
sama dan `Cache-Control: no-store`. Aset dashboard dan font dibundel dalam binary.

Dashboard memuat ulang data setiap 30 detik saat tab aktif dan tidak ada form
terbuka. Mutasi tidak dicoba ulang otomatis; refresh yang lebih lama tidak boleh
menimpa hasil perubahan akun yang baru disimpan.

## Dashboard pengguna

`/u` menyediakan form API key, dan `/u/{key}` membuka dashboard akun dengan
ringkasan, grafik token rolling 24 jam/7 hari, riwayat request, filter model,
pagination (50 baris), ekspor CSV halaman aktif, serta panduan API. Semua statistik
diambil dari database motgate; tidak ada data contoh di halaman produksi. Grafik
mengikuti token yang dilaporkan upstream, sehingga nilai nol tetap ditampilkan
saat upstream tidak menyertakan usage. Data diperbarui setiap 30 detik saat tab aktif.

API key disamarkan sampai pengguna memilih tampilkan/salin. Contoh kode memakai
`ARBYTE_API_KEY` dari environment, menggunakan streaming dan pesan system pertama.
Halaman akun dan API statistik menggunakan `Cache-Control: no-store`; halaman
menggunakan `Referrer-Policy: no-referrer`. Font Inter dan JetBrains Mono dibundel
di binary Go di `/assets/fonts/`, bersama lisensi masing-masing.

## Katalog landing dan model per paket

`GET /api/admin/catalog` dan `PUT /api/admin/catalog` memakai autentikasi admin.
Kartu paket mendukung `model_scope: "all"` atau `"selected"` dengan `models: ["model-id"]`.
Paket lama tanpa pilihan scope tetap menampilkan semua model aktif. Pilihan ini
mengatur daftar model pada kartu landing; pengaturan akses API akun tetap dikelola
oleh paket akun dan verifikasi metering. API menolak pilihan kosong, ID yang tidak
ada di katalog, dan ID duplikat pada scope `selected`.

Admin package model selection also queries `GET /api/admin/catalog/router-models`
(authenticated). Set `MOTGATE_MODEL_CATALOG_URL` to the router base URL; the default
is `http://127.0.0.1:8787`. The catalogue query uses `/v1/models`, a 15-second
request deadline, a 2 MiB response cap and at most 200 unique valid model IDs.
It makes no inference requests. Search matches model name, ID and provider.
Selecting a new router model adds it to the editable landing catalogue; changes
are published only after Save. Existing package prices and selections stay intact.
Listing a model does not verify credentials, free access or upstream health.
Public page visitors never trigger this live router catalogue query.
