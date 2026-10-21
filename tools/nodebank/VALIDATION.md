# Mot NodeBank — hasil validasi

Tanggal: 2026-10-12T03:00:41+07:00. Linux x86_64, Intel(R) Celeron(R) N5100 @ 1.10GHz, RAM fisik 7.56 GiB. Laptop menjalankan aplikasi lain selama benchmark; angka waktu bukan kondisi CPU/disk terisolasi.

## Impor bank sintetis

Sembilan kasus lulus. Setiap kasus memakai bank SQLite terpisah dan input di disk. RSS dibaca dari /proc setiap 50 ms; angka ini **engine CLI saja**, tanpa worker. Generator mengalirkan file; tidak menampung bank di RAM. URI menambah duplikat serta konfigurasi rusak: data rusak berulang masuk satu identitas unsupported, bukan proxy mati. Base64 dibuka ke disk; YAML penuh diawali mixed-port dan memuat proxies per record.

| Kandidat valid | Format | Waktu (detik) | Peak RSS (MiB) | Node/detik | CPU proses (detik) |
|---:|---|---:|---:|---:|---:|
| 100,000 | uri | 16.450 | 24.25 | 6079.0 | 8.48 |
| 100,000 | base64 | 16.407 | 24.38 | 6094.9 | 8.82 |
| 100,000 | yaml | 16.132 | 24.38 | 6198.8 | 9.53 |
| 500,000 | uri | 168.478 | 24.25 | 2967.7 | 56.68 |
| 500,000 | base64 | 125.129 | 24.38 | 3995.9 | 54.19 |
| 500,000 | yaml | 119.983 | 24.25 | 4167.2 | 55.15 |
| 1,000,000 | uri | 258.041 | 24.38 | 3875.4 | 113.33 |
| 1,000,000 | base64 | 212.330 | 23.75 | 4709.6 | 99.25 |
| 1,000,000 | yaml | 260.851 | 23.75 | 3833.6 | 116.62 |

Data mentah: [benchmark-import.json](benchmark-import.json). Pengukuran sebelum pengurangan indeks yang tidak diperlukan tersimpan terpisah pada benchmark-import-initial-indexes.json; satu run di sana dihentikan untuk mengganti profil dan bukan hasil penerimaan.

## Runtime engine + worker

Release default memeriksa 500 node HTTP CONNECT terhadap HTTPS lokal dengan CA fixture dipercaya, lalu mengimpor 100.000 node sambil menguji jeda/lanjut. Worker merupakan proses Go terpisah. RSS menjumlahkan engine dan child worker, diambil tiap 50 ms dari /proc.

- Peak RSS gabungan: **52.76 MiB** (di bawah target 256 MiB dan puncak 384 MiB).
- Pemeriksaan lokal: **62.66 node/detik**, 7.98 detik untuk 500 node.
- Respons statistik paling lambat: **13.73 ms** selama pemeriksaan.
- Peak tunnel fixture bersamaan: **19**. Nilai ini bukan total seluruh socket TCP, karena mencakup tunnel aktif dan tidak menghitung listener maupun TCP preflight.
- CPU gabungan proses awal: **11.35 detik**, mencakup pemeriksaan, impor dan pemulihan awal; CPU server Python fixture tidak dihitung.

Lulus: startup dijeda, sumber awal nonaktif, penolakan permintaan lintas situs, pagination maksimal 200, ekspor lengkap/deduplikasi IP/0600, pause/resume impor dan scan, endpoint gagal tidak merusak status stok, worker crash mengembalikan pekerjaan, engine SIGKILL mengembalikan lease dan pekerjaan menjadi paused. Data mentah: [benchmark-runtime.json](benchmark-runtime.json).

## Parser, adapter dan ketahanan

12 tes Rust lulus: URI/Reality/transport, streaming YAML flow/block/nested, angka kredensial dan short ID berawalan nol, deteksi YAML Clash, base64, record terlalu besar, unsupported, batas tekanan memori, dedup konfigurasi, cursor pagination, ekspor atomik/freshness, SQLite busy tanpa kehilangan checkpoint, recovery, batas antrean dan pemangkasan riwayat.

Tes Go lulus untuk HTTP/SOCKS5/Shadowsocks/VMess/VLESS/Trojan, TLS, WS, gRPC, Reality/Vision; autentikasi salah, respons terlalu besar, status tujuan salah, sertifikat tujuan tidak dipercaya, koneksi putus dan timeout TCP/WS/gRPC ditolak. Reverse proxy fixture membuktikan Host WebSocket tetap localhost:port ketika dial menggunakan IP hasil preflight. Deadline menutup socket mentah, bukan hanya menunggu HTTP client.

Tekanan memori diuji dengan nilai ambang sintetis; laptop tidak sengaja dibuat OOM. Profil systemd MemoryHigh=256M, MemoryMax=384M disediakan tetapi tidak dipasang atau dites sebagai service produksi. GOMAXPROCS=2 membatasi eksekusi Go serentak; bukan batas dua thread OS total.

## Sampel publik

Lima URI pertama dari MORPHEUS-best diimpor pada bank sementara terpisah. Hasil: 0 healthy, 4 failed, 1 unsupported. Empat VLESS menjalani pemeriksaan TCP/protokol/HTTPS tetapi tidak berhasil; satu memuat opsi URI yang belum dimodelkan. Ini menguji alur kegagalan pada proxy nyata dan **belum membuktikan satu kandidat publik sehat**. Tidak ada pemindaian subscription penuh, prompt Zen atau tes bandwidth.

Hasil tanpa alamat/kredensial: [validation-real.json](validation-real.json). Bank dan URI sampel privat dibersihkan setelah pengujian. Fixture lokal yang sukses memberikan bukti positif untuk adapter dan ekspor; hasil publik tetap harus diuji ulang dari VPS.

## Dashboard

Dashboard 127.0.0.1:8792 dibuka di browser in-app. Ringkasan, sumber nonaktif, filter bank, kandidat dan pekerjaan diperiksa; console tidak mencatat error/warning. Aset dibundel lokal, tampilan mengikuti ukuran panel browser. Engine final ditinggalkan berjalan dalam keadaan dijeda dengan bank kosong; data sintetis tidak disalin ke bank pengguna.

Target memori terpenuhi untuk fixture dan impor yang diukur. Hasil runtime ini mencakup HTTP CONNECT lokal; memory peak semua kombinasi protokol secara serentak pada skala maksimal 32 belum diukur. Pemeriksaan TLS/WS/gRPC/Reality/Vision dilakukan pada fixture adapter terpisah. Angka throughput publik akan bergantung pada latensi serta kualitas stok.
