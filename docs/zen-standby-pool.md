# Rotasi dan tes Zen setelah pengembalian

Fitur pool cadangan Zen serta pemeriksaan sepuluh IP dihapus atas permintaan
pengguna pada 17 Oktober 2026. Router kembali memakai rotasi node serial.

- **Rotate sekarang** memilih node sehat berikutnya dari inventaris NodeBank.
- **Tes ke Zen** menguji satu node aktif memakai `deepseek-v4.1-flash-free` dan profil
  adaptasi tersimpan; tes tidak mengganti node atau mencoba sepuluh IP.
- Tidak ada scheduler pool, budget probe pool, cache cadangan per model,
  maupun penguncian cooldown per model dari fitur tersebut.
- NodeBank tetap mengimpor dan memeriksa kesehatan koneksi sebagaimana sebelumnya.
- Error autentikasi, akses dan kuota upstream tetap diteruskan tanpa rotasi otomatis.

`deployments/zen-pool-remove.sh` membackup binary, konfigurasi dan SQLite secara
konsisten, kemudian memasang router baru. Runtime NodeBank tidak direstart.
Database live tidak dipulihkan dari backup saat rollback binary agar transaksi
pelanggan tetap tersimpan. Setting pool lama menjadi data tidak aktif dan
tidak digunakan oleh router. Catatan pengujian lama disimpan sebagai riwayat.
