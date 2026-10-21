# Mot NodeBank

Engine bank proxy **laptop** yang terpisah dari Mot Router dan `ipfarm`. Rust/Tokio menyimpan bank di SQLite, sedangkan satu worker Go membuat adapter sing-box per pemeriksaan. Tidak memakai Mihomo atau memuat seluruh bank ke RAM. Tidak merutekan trafik AI, memanggil Zen, atau mengubah service Mot Router.

## Jalankan

Dibutuhkan Rust stable, Go ≥1.25.5, compiler C untuk SQLite bundled, Linux amd64/arm64, dan koneksi internet saat build pertama.

```bash
cd /home/rian/Desktop/mot-router/tools/nodebank
./build.sh
./run.sh
```

Dashboard: **http://127.0.0.1:8792**. Pemeriksaan selalu **dijeda saat startup**, termasuk setelah restart. Semua sumber awal nonaktif. Aktifkan sumber pilihan lalu tekan **Impor**, atau unggah file. Impor tidak otomatis memulai pemeriksaan. Gunakan Mulai/Jeda/Lanjutkan, dan Pekerjaan untuk menjeda impor setelah transaksi berjalan selesai.

Dashboard VPS melalui domain: **https://proxy.coddx.store**. Konfigurasi Caddy ada di `../../deploy/Caddyfile.nodebank`; ganti placeholder hash dengan hash login `rian` yang sudah digunakan `mot.coddx.store`. DNS Cloudflare harus memiliki record A `proxy` ke `43.159.58.17`, mode Proxied, dengan kebijakan TLS origin yang sama seperti domain Mot Router. Login menggunakan akun dashboard Mot Router. Backend tetap loopback; Caddy memeriksa Origin sebelum menyesuaikan Host/Origin untuk guard lokal. Endpoint privat `/api/runtime/*` ditolak dari domain, dan SSE diteruskan tanpa buffering.

```bash
# Impor CLI menggunakan bank lain atau hentikan engine bank tersebut lebih dahulu.
./dist/mot-nodebank --data /tmp/my-bank import /path/subscription.txt
./dist/mot-nodebank --data /tmp/my-bank stats
./dist/mot-nodebank --data /tmp/my-bank export --output /tmp/my-bank/healthy-v1.json
```

`NODEBANK_DATA` mengubah direktori data pada run.sh. `--bind`, `--worker`, `--health-url`, `--exit-url`, `--expected-status`, `--check-ca` (CA tambahan yang dipercaya) tersedia untuk serve. Target harus HTTPS. Default health adalah gstatic/generate_204 (204), IP keluar api.ipify.org (200). TLS tujuan selalu diverifikasi; `insecure` dari subscription hanya berlaku untuk TLS server proxy.

## Penyimpanan dan resource

- URI/gzip/base64 diproses dengan buffer terbatas. Subscription base64 dibuka ke file sementara di disk; YAML menggunakan event parser satu proxy per waktu. Maksimal input/hasil decoding 512 MiB, proxy 64 KiB, baris YAML 1 MiB, nesting 16, anchors 256. Bank tidak dikoleksi ke Vec di RAM.
- SHA-256 dari JSON konfigurasi koneksi kanonik mengabaikan nama/sumber. Host yang sama dengan UUID/password/SNI/transport berbeda tetap node berbeda.
- Transaksi 500 record, lease batch 128, antrean IPC 32, maksimal worker aktif 32. Cache TCP berbagi hasil endpoint dan IP yang berhasil selama 60 detik, maksimal 8.192 endpoint; TCP 3 detik, protokol+HTTPS+IP 15 detik, body/header tujuan maksimal 16 KiB.
- SQLite WAL, cache 16 MiB, mmap mati, temp sort di disk, checkpoint otomatis 1.000 halaman dan sesudah impor. O(1) status counters. Riwayat hanya agregat sukses/gagal, EWMA latensi, hasil terakhir; tidak menambah satu baris per probe.
- 128 TCP / 16 protokol awal. Bertambah perlahan sampai 256 / 32 ketika RAM cukup. Diturunkan menjadi 64 / 8 di atas 220 MiB gabungan. Di 256 MiB gabungan atau MemAvailable <768 MiB, pekerjaan baru dijeda. Baseline koneksi/endpoint diverifikasi sebelum scan dan secara berkala; batch kegagalan protokol tidak dikomit bila baseline gagal.
- Tokio menggunakan dua thread runtime dan dua blocking thread; Go GOMAXPROCS=2 membatasi thread yang mengeksekusi Go secara serentak (runtime tetap dapat membuat thread OS tambahan untuk I/O/GC). Go memory limit 96 MiB adalah batas lunak GC. Pembatas keras **gabungan** disediakan oleh cgroup systemd 384 MiB, tidak melalui ukuran heap Go saja.
- Scan manual selesai saat tidak ada pekerjaan yang jatuh tempo. Mode background menunggu dan memeriksa node sehat tiap 30 menit. Gagal memakai backoff hingga enam jam. Mode background harus dipilih sebelum memulai scan.
- Lease dilepas saat worker gagal/macat; startup mengembalikan lease dan menjeda pekerjaan yang terputus. Resume impor membaca ulang file sampai checkpoint tanpa memasukkan ulang record yang sudah dikomit. File unggahan/unduhan sukses dan hasil decoding dihapus; file CLI milik pengguna dipertahankan.
- Maksimal delapan pekerjaan belum selesai, satu upload/download aktif, 100 sumber, 100 pekerjaan terakhir pada UI. Database bank memang bertambah sesuai stok di disk. Sediakan ruang disk cukup; impor ditolak saat ruang bebas rendah.

Memori target adalah **gabungan RSS engine + worker sekitar 256 MiB**, peak <384 MiB. Baca `VALIDATION.md` untuk hasil pengukuran, bukan asumsi berdasarkan bahasa pemrograman.

## Protokol

VLESS, VMess, Trojan, Shadowsocks AEAD/2022, HTTP CONNECT termasuk HTTPS proxy, SOCKS5. TLS/SNI, uTLS fingerprint, Reality, Vision, WS/early data, gRPC, HTTP/H2 dan HTTPUpgrade diteruskan ke adapter. Pemeriksaan hanya TCP dan HTTP(S), bukan UDP.

Kombinasi yang adapter tolak disimpan sebagai **unsupported**, bukan gagal jaringan. KCP/QUIC/XHTTP, plugin SS, cipher lama, multiplex khusus, dialer-proxy, alias YAML di dalam proxy dan opsi koneksi yang tidak dimodelkan tidak didukung pada versi ini. YAML harus memiliki top-level `proxies` (format Clash/provider). Label/error ditampilkan tanpa konfigurasi rahasia; detail kredensial hanya ada di database dan ekspor privat.

## Ekspor

`healthy-v1.json`: schema_version=1, generated_at epoch detik UTC, vantage=laptop, zen_verified=false; node menyertakan config lengkap, latency_ms, exit_ip, checked_at, ok/fail, score=(ok+1)/(ok+fail+2). Hingga 300 node, satu per exit IP, sukses dalam satu jam, latency ≤3.000 ms, urutan score → latensi → kesegaran. Kandidat disebut **sehat dari laptop**; VPS harus memeriksa ulang sebelum digunakan untuk Zen.

Publikasi 0600 menggunakan file sementara, fsync, rename atomik. Ekspor kosong gagal dan mempertahankan file valid sebelumnya. Download dashboard membutuhkan header khusus agar tidak terekspos melalui permintaan lintas situs. Bank 0700, db/export 0600; service memakai umask 077. Dashboard hanya bind loopback, menolak Origin/fetch lintas situs dan Host nonlokal.

## Jembatan SSH ke VPS (snapshot saja)

Tab Kandidat terbaik memiliki tombol **Kirim snapshot ke VPS**. Engine membuat ekspor terbaru lalu mengirimkannya lewat `ssh -T mot`; host `mot` harus berupa alias atau host SSH yang sudah dikonfigurasi di laptop dan autentikasi harus noninteraktif (`BatchMode`). Tidak ada port VPS baru yang dibuka. Target dapat diganti dengan `NODEBANK_VPS` sebelum service dijalankan, memakai alias/host atau `user@host` tanpa argumen tambahan.

Penerima satu-kali ada di `vps/receive.py` dan dipasang di VPS sebagai `/home/ubuntu/.local/libexec/mot-nodebank/receive.py`. Snapshot disimpan di `/opt/mot-router/data/nodebank/healthy-v1.json`, direktori mode 0700 dan file mode 0600. Penerima menolak schema/provenance salah, snapshot kosong/kedaluwarsa, konfigurasi/protokol tidak didukung, node lebih dari 5.000, IP keluar ganda, atau payload di atas 32 MiB; publikasi memakai file sementara, fsync, dan rename atomik. File sebelumnya tetap ada ketika validasi gagal. `python3 vps/test_receive.py` menjalankan tes lokal.

Penerima SSH hanya menyimpan snapshot; pemeriksaan dan pemuatan ke grup MOT ditangani runtime native yang dijelaskan di bawah. Kandidat harus lolos pemeriksaan VPS sebelum dapat dipilih untuk trafik.

## API lokal

- GET `/api/stats`, `/api/events` (SSE event `stats` tiap dua detik)
- GET/POST `/api/sources`; POST `/api/sources/{id}/import`
- POST `/api/upload` multipart field `file`
- GET `/api/jobs`, `/api/jobs/{id}`; POST `/api/jobs/{id}/control` body `{"action":"pause|resume|cancel"}`
- POST `/api/scan` body `{"background":false}`
- GET `/api/nodes?status=&protocol=&source=&after=&limit=100` keyset pagination, cap 200
- GET `/api/best`; POST `/api/export`; GET `/api/export/download` dengan `X-NodeBank-Export: 1`

Operasi impor/scan/export menghasilkan `job_id`, progres persisten melalui GET jobs; SSE untuk statistik cepat dan ringkasan progres pekerjaan. IPC JSONL stdin/stdout cap 128 KiB: request id/config/resolved_ip/deadline_ms/timeout_ms/health_url/exit_url/expected_status, result id/status/latency_ms/exit_ip/error. Stdout worker hanya JSON; pesan tidak mengandung kredensial.

## systemd user (opsional)

Template `systemd/mot-nodebank.service` sudah mengikat **engine dan worker** dalam satu cgroup: MemoryHigh=256M, MemoryMax=384M, tanpa swap, CPUQuota=150%, Nice=10, TasksMax=64. Tidak dipasang otomatis.

```bash
mkdir -p ~/.config/systemd/user
cp systemd/mot-nodebank.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now mot-nodebank
```

Service yang terkena hard limit akan restart dalam keadaan dijeda. Jangan jalankan run.sh dan service untuk bank yang sama; file lock mencegah dua pemilik bank.

## Tes dan benchmark

```bash
cargo test --locked
(cd probe && go test -buildvcs=false -p 2 -tags with_utls,with_grpc -v ./...)
python3 scripts/benchmark_import.py --sizes 100000,500000,1000000 --formats uri,base64,yaml
python3 scripts/integration.py
```

Fixture/benchmark menggunakan bank sementara dan listener lokal; benchmark impor tidak membuka koneksi proxy. Semua dependensi terkunci di Cargo.lock/go.sum. Worker Go memakai sing-box v1.14.3, buildtags with_utls,with_grpc. Rust manager berlisensi MIT, worker beserta sing-box berlisensi GPL-3.0-or-later; lihat LICENSE dan probe/LICENSE. Receiver SSH dan runtime native tersedia; lihat bagian runtime VPS di bawah.

Konfigurasi adapter mengacu pada [outbound sing-box](https://sing-box.sagernet.org/configuration/outbound/) dan [VLESS](https://sing-box.sagernet.org/configuration/outbound/vless/), dengan API implementasi dipin ke v1.14.3. File `assets/source-references.json` menyimpan repository dari dokumen yang belum memiliki URL subscription spesifik; dashboard menampilkannya sebagai referensi, tanpa mengimpor HTML repository sebagai node.

Pagination memakai cursor baris SQLite agar filter tidak melakukan pembacaan acak seluruh bank menurut hash. `after` adalah cursor opaque dari `next_cursor`. Mode background disimpan pada pekerjaan sehingga tetap dapat dilanjutkan setelah restart (startup tetap dijeda).

## Runtime native untuk grup MOT di VPS

`nodebank-probe runtime` menyediakan engine tunnel HTTPS menggunakan adapter yang sama dengan pemeriksa. Tidak menjalankan Mihomo dan tidak mengambil kode/antrean ipfarm. Control API kompatibel dengan antarmuka rotasi router pada loopback `9093`, proxy CONNECT pada `7893`. Di router, `MOT_NODEBANK_RUNTIME=1` mengarahkan integrasi ke engine ini dan memilih grup MOT.

Pada VPS yang menjalankan dashboard NodeBank, runtime membaca `/api/runtime/candidates` dengan header `X-NodeBank-Runtime: 1`. Siklus berikutnya dimulai satu menit setelah siklus sebelumnya selesai; pemeriksaan memakai empat worker. Endpoint privat ini mengirim maksimal 5.000 konfigurasi sehat menurut bank, satu per IP keluar, untuk **diperiksa ulang** dari VPS. Status runtime memisahkan `candidate_cap`, jumlah kandidat, `unique_healthy_exit_ips`, dan `target_gap`. Daftar umum/dashboard tetap menyamarkan kredensial. Runtime dapat juga membaca snapshot `--snapshot` untuk penerima laptop yang tidak menjalankan bank lokal. Ekspor dashboard tetap default 300; CLI `export --limit 5000` dapat menghasilkan hingga 5.000 kandidat.

`POST /api/sources/{id}/enabled` menerima hanya `{"enabled": true}` atau false untuk mengaktifkan/nonaktifkan sumber yang sudah terdaftar. Endpoint ini mempertahankan URL, kredensial, interval, dan data impor; menonaktifkan sumber menghentikan impor berikutnya tanpa menghapus node atau membatalkan pekerjaan yang sedang berjalan. Agent mengaksesnya lewat `arbytectl sources enable/disable SOURCE_ID`.

Node dimulai sebagai pending dan belum selectable. Empat pemeriksa menjalankan HTTPS dengan sertifikat tujuan diverifikasi dan pembacaan IP kecil dari VPS; pengecekan ini tidak mengirim prompt Zen. Node yang lolos diperiksa lagi setelah 30 menit, kegagalan memakai backoff hingga enam jam. Kegagalan koneksi dasar/endpoint menghentikan pemeriksaan tanpa menandai seluruh kandidat mati. RSS runtime di atas 96 MiB atau RAM tersedia di bawah 128 MiB menghentikan pekerjaan baru. Status `/status` menyampaikan alasan, penghitung dan jumlah tunnel aktif.

Grup MOT mulai dengan DIRECT, mempertahankan jalur VPS saat ini. Kandidat yang lolos dapat dipilih manual atau lewat rotasi router. Status native menampilkan IP hasil VPS; penalti limit juga melewati node lain dengan IP keluar yang sama. Klaim sehat berarti lolos pemeriksaan jaringan, bukan jaminan diterima Zen. Node dipilih per percobaan melalui metadata CONNECT lokal, sehingga probe/request lain yang mengubah grup tidak mengalihkan percobaan tersebut atau memberi penalti ke node yang salah. Perubahan grup tidak memotong stream yang sudah berjalan; batas dial 15 detik tidak membatasi panjang generasi.

Contoh menjalankan runtime:

```sh
./dist/nodebank-probe runtime --bank-url http://127.0.0.1:8792 \
  --snapshot data/healthy-v1.json --controller 127.0.0.1:9093 --proxy 127.0.0.1:7893
```

Template VPS ada di `vps/mot-nodebank-runtime.service` dan `vps/mot-nodebank.slice`. Bank, worker pemeriksaan dan runtime bergabung dalam satu slice dengan MemoryHigh=256M / MemoryMax=384M. Runtime sendiri dibatasi 160M dan 64 tunnel aktif; listener dibatasi 96 koneksi. Semua port tetap loopback. Angka ini merupakan batas operasional, bukan bukti benchmark satu juta node.

Playground dan tombol tes Zen memakai jalur MOT yang sama dengan inference ketika routing diaktifkan, termasuk retry/rotasi dan penalti IP.
