# guard — admission control untuk mot-router

Proses C++ yang berdiri di depan mot-router: membatasi berapa banyak request yang benar-benar
menyentuh mot-router/Zen ketika banyak user memakai portal bersamaan.

```
client -> motgate :8790 -> guard :8791 -> mot-router :8787 -> mihomo :7890 -> Zen
```

## Kenapa

mot-router tidak punya rate limiter (hanya klasifikasi `isRateLimited` untuk error upstream).
Rotasi IP (ipfarm/mihomo) mengatasi limit **per-IP**; guard mengatasi limit **per-user dan
global** supaya satu user yang menembak ratusan request tidak memonopoli kuota semua orang.

## Arsitektur

Fix thread pool sebesar `--max-inflight`; tiap worker menangani tepat satu koneksi. Karena itu
jumlah request yang benar-benar diteruskan ke mot-router **tidak pernah** melebihi
`--max-inflight`. Saat slot akun penuh, request tambahan menunggu dalam antrean FIFO
yang dibatasi. Batas default tetap empat inferensi aktif per akun; antrean tidak
menaikkan jumlah request yang diteruskan bersamaan.

## Flag

| Flag | Default | Arti |
|---|---|---|
| `--listen` | `127.0.0.1:8791` | alamat bind (env `GUARD_LISTEN`) |
| `--upstream` | `http://127.0.0.1:8787` | mot-router |
| `--master-key` | — | **wajib**; key `mr_` yang dipakai guard ke mot-router (env `GUARD_MASTER_KEY`) |
| `--max-inflight` | 64 | worker = request paralel maksimum |
| `--per-user-inflight` | 4 | request paralel per user |
| `--global-rps` | 40 | token bucket global |
| `--user-rps` | 8 | token bucket per user |
| `--queue-max` | 512 | listen backlog |
| `--queue-wait-ms` | 60000 | batas tunggu slot per akun; 0 mematikan antrean, maksimal 120000 |
| `--per-user-queue` | 8 | batas request menunggu per akun; maksimal 32 |
| `--retries` | 2 | ulangan saat upstream menjawab 429/502/503 sebelum ada byte body |
| `--retry-backoff-ms` | 300 | backoff awal, dikali 3 tiap percobaan |

Tanpa `--master-key` proses keluar dengan kode 2.

## Perilaku

- Membutuhkan header `X-Motgate-User`; tanpa itu → `400`. Header itulah kunci pembatasan
  per-user.
- Antrean memakai worker yang sudah ada, tanpa membuat thread tambahan. Maksimal
  separuh worker boleh menunggu (32 dari 64 secara default); payload yang ditahan
  antrean dibatasi gabungan 16 MiB. Akun lain tetap dapat masuk saat satu akun
  menunggu. Klien yang terputus saat menunggu dikeluarkan tanpa diteruskan upstream.
- Jika slot tersedia sebelum deadline, request diteruskan sekali dengan body/model
  asli dan stream diteruskan normal. Jika antrean penuh atau waktu tunggu habis,
  respons tetap 429 dengan teks yang menjelaskan kondisi serta `Retry-After: 1`.
  Error upstream dan catatan pemakaian tetap dipertahankan; tidak ada respons sukses palsu.
- `429` body: `{"type":"error","error":{"type":"rate_limit_error","message":"..."}}`.
- Respons upstream di-stream apa adanya memakai HTTP/1.1 chunked (`Transfer-Encoding: chunked`);
  header upstream yang diteruskan hanya `Content-Type`. SSE tidak dibuffer.
- Response interim HTTP `1xx` (termasuk `100 Continue`) tidak diteruskan sebagai
  response final; permintaan klien dengan `Expect: 100-continue` di-ack sebelum
  Guard membaca body, agar klien HTTP Go tidak mengira panjang chunk stream
  seperti `2a0` sebagai status HTTP baru.
- Status head baru ditulis setelah blok header upstream selesai; itu membuat status
  retryable (429/502/503) bisa dibuang dan diulang tanpa merusak stream.
- `GET /healthz` → `{"ok":true,"inflight":N,"queued":N,"rejected":N,"upstream_errors":N,
  "max_inflight":N,"per_user_inflight":N,"queue_wait_ms":N,"waited":N,"queue_timeouts":N}`.
- `SIGPIPE` diabaikan supaya client yang memutus stream tidak mematikan proses.

## Build & jalankan

```bash
make -C tools/guard              # butuh /usr/bin/g++-10 + libcurl
GUARD_MASTER_KEY=mr_... ./guard --max-inflight 64 --per-user-inflight 4
```

## Deploy

`deploy/deploy-portal.sh` memasang `deploy/systemd/guard.service`. Lihat
`tools/motgate/README.md` untuk gambaran menyeluruh.

## Tes antrean

```bash
make -C tools/guard test CXX=/usr/bin/g++
```

Tes memakai server fixture lokal, tanpa prompt AI produksi: burst streaming dan
JSON, FIFO, isolasi akun, timeout, batas jumlah/ukuran antrean, pembatalan koneksi,
serta pemulihan slot. Build membutuhkan header development libcurl. Binary untuk
Ubuntu VPS dibuat dengan toolchain native VPS agar cocok dengan glibc/libstdc++.
