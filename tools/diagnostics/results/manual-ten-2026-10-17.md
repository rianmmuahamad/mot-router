# Tombol Rotasi IP / Tes Zen — 17 Oktober 2026

Kedua tombol pada panel Rotasi IP kini memakai batch maksimal sepuluh IP unik,
terlepas dari paralelisme pemeriksaan latar belakang. Ada dropdown model Zen dan
tabel hasil per IP. Test tidak mengganti node; Rotate memilih alternatif yang
lulus dengan latency terbaik setelah batch. Budget 120/jam, cooldown upstream,
reserve 16 tunnel, dan batas RAM tetap berlaku. Klik bersamaan berbagi batch;
perubahan pilihan operator dilindungi compare-and-swap di runtime.

## Bukti

- Tes race router/server/store/Zen lulus; tes native runtime dengan utls/grpc
  lulus. JavaScript dashboard lulus pemeriksaan sintaks.
- Fixture: sepuluh probe simultan saat background disetel 1/dijeda; test tidak
  mengubah pilihan; rotate memilih alternatif tercepat; quota menghentikan batch
  dan klik ulang; beberapa klik berbagi satu batch; pilihan baru tidak tertimpa.
- Upgrade membawa pengukuran VPS segar untuk hash konfigurasi yang cocok.
  Freshness, konfigurasi berubah, dan pemilihan atomik diuji.
- Produksi **Tes ke Zen**: paralel 10, diperiksa 10, lulus 9, waktu 13.548 ms,
  node aktif tetap.
- Produksi **Rotate sekarang**: paralel 10, diperiksa 10, lulus 10, waktu
  11.322 ms; terpilih `NB-a240bf858617c2f573e90371`, IP `52.221.223.83`.
- Model kedua sampel: `deepseek-v4.1-flash-free`. Tidak ada prompt pelanggan
  yang direplay; hanya probe instruksi tetap kecil.
- Pengaturan background tetap enabled/paralel 1. Kredensial tidak diubah.
- RSS router + runtime sebelum sampel 42.896 KiB (41,89 MiB); sesudah sampel
  53.820 KiB (52,56 MiB). Peak sampling selama batch 53.476 KiB (52,22 MiB).
  Ini pengukuran sampel terbatas, bukan klaim peak untuk semua beban produksi.
- Backup: `/var/backups/mot-router/manual-ten-20261017T005526Z`.

Metadata lengkap: `manual-ten-production-2026-10-17.json`.
