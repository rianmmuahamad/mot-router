# Pengembalian rotasi serial — 17 Oktober 2026

Permintaan: hapus seluruh fitur pemeriksaan sepuluh IP dan kembalikan perilaku sebelumnya.

Dihapus: pool cadangan Zen, scheduler/probe paralel, cache dan penguncian cooldown
per model, API pool, form pemilihan model tes, serta tampilan hasil batch.
Rotate sekarang kembali memilih node sehat berikutnya. Tes ke Zen kembali menguji
satu node aktif, tanpa seleksi node atau retry ke kandidat lain.

Validasi lokal: `go test -short -race -p 2 -buildvcs=false -timeout 60s ./...`
lulus. Fixture TLS/CONNECT membuktikan tepat satu request inferensi, node tetap,
serta error 403/429 dan stream terputus tetap dilaporkan. Setting pool lama yang
masih aktif dengan cooldown masa depan tidak mengubah jalur single-node.
Syntax JavaScript dan simulasi interaksi kedua tombol lulus.
Percobaan awal suite tanpa `-short` menjalankan TestProbeLive; akses jaringan
ke upstream gagal dengan `no route to host`. Tes live tersebut tidak diulang.

Deployment: binary Go statis root saja; runtime NodeBank tidak direstart.
API `/api/zen-pool` mengembalikan 404 dan halaman tidak memuat komponen pool/batch.
Node aktif tetap `NB-a240bf858617c2f573e90371`, IP sehat VPS 577 saat validasi.
Backup: `/var/backups/mot-router/pool-remove-20261017T020145Z`.
SHA256 binary: `a3bddaf08f876cf2d51d0a0a86a33056bb132603033a0876da471718b4de6388`.
Tidak mengirim probe inferensi produksi tambahan.
