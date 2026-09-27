# Arsitektur IP Rotation

Dokumen ini menjelaskan bagaimana Mot Router memindahkan IP egress saat upstream
menolak request, kenapa dirancang begitu, dan apa yang sudah diukur.

Semua angka di dokumen ini berasal dari pengukuran di mesin ini, bukan perkiraan.

---

## 1. Masalah yang dipecahkan

Zen free tier membatasi **per IP**, bukan per akun. Satu IP yang sudah kena limit
akan membalas `429 FreeUsageLimitError` untuk **semua** request, sekalipun
request-nya valid.

Konsekuensinya:

- Ganti API key tidak menolong — limitnya bukan di kredensial.
- Tunggu saja bisa, tapi tidak dapat diprediksi.
- Satu-satunya cara yang benar-benar mereset kuota adalah **mengganti IP egress**.

Jadi rotasi IP bukan optimasi, melainkan mekanisme inti supaya router tetap bisa
melayani.

---

## 2. Dua jalur rotasi

Router punya dua mekanisme, dan **mihomo yang dipakai** karena hanya itu yang
bisa mengganti IP sungguhan.

| | Cloudflare Worker relay | mihomo (Clash Meta) |
|---|---|---|
| Cara ganti IP | deploy Worker baru | pindah node di group |
| Sifat IP | **bersama** semua pengguna gratis | banyak node, IP berbeda |
| Hasil untuk Zen | selalu `429` | berhasil |
| Status | ada, **nonaktif** | **aktif** |

Cloudflare Worker gagal untuk Zen karena IP egress-nya dipakai bersama seluruh
pengguna gratis Cloudflare dan sudah jenuh. Terukur: relay selalu `429`,
sementara jalur langsung justru `200`. Relay tetap terpasang sebagai jalur cadangan.

**Urutan pemilihan** (`internal/server/server.go`):

```
mihomoStreamZen()  ──► kalau mihomo aktif dan group bisa dipilih
      │
      └─ gagal ──► streamZenWithRotation()  ──► relay Cloudflare
```

Kalau mihomo mengembalikan nama group, berarti jalur mihomo yang dipakai.
Kalau tidak, router jatuh ke relay.

---

## 3. Alur lengkap satu request

```
                            Klien
                              │
                              ▼
                  POST /v1/chat/completions
                              │
                              ▼
            ┌─────────────────────────────────────┐
            │  Adaptasi gate (header + tool)      │
            │  - User-Agent: opencode/1.18.x      │
            │  - session: ses_<12hex><14alnum>    │
            │  - body: "stream": true             │
            │  - tool wajib: bash, read           │
            └─────────────────────────────────────┘
                              │
                              ▼
                  mihomo aktif & group valid?
                        │              │
                       ya             tidak
                        │              │
                        ▼              ▼
            ┌───────────────┐   ┌──────────────┐
            │ mihomoStream  │   │ relay        │
            │ Zen()         │   │ Cloudflare   │
            └───────┬───────┘   └──────────────┘
                    │
                    ▼
    ┌───────────────────────────────────────────────┐
    │  LOOP — maks 4 percobaan, jatah 90 detik      │
    │                                               │
    │   attempt 0 ──► kirim lewat proxy mihomo      │
    │       │                                       │
    │       ├── ada respons ──► tunggu event        │
    │       │                    pertama (30 dtk)   │
    │       │                          │            │
    │       │                     ada event         │
    │       │                          │            │
    │       │                          ▼            │
    │       │                  KEMBALIKAN ke klien  │
    │       │                                       │
    │       └── gagal ──► shouldRotate()?           │
    │                        │           │          │
    │                      tidak        ya          │
    │                        │           │          │
    │                        ▼           ▼          │
    │                   berhenti    rotasi aktif?   │
    │                   + error       │       │     │
    │                                ya      tidak  │
    │                                 │       │     │
    │                                 │       ▼     │
    │                                 │  ipTiedFailure?
    │                                 │    │      │ │
    │                                 │   ya     tidak
    │                                 │    │      │ │
    │                                 │    ▼      │ │
    │                                 │ berhenti  │ │
    │                                 │ + error   │ │
    │                                 ▼           │ │
    │                          ROTASI node baru   │ │
    │                          + ResetConnections │ │
    │                                 │           │ │
    │                                 │   RETRY node
    │                                 │   yang sama │
    │                                 │           │ │
    │                                 └─────┬─────┘ │
    │                                       │       │
    │                            attempt 1, 2, 3    │
    └───────────────────────────────────────────────┘
                              │
                              ▼
                        error ke klien
```

Inti percabangan setelah kegagalan:

| Pertanyaan | Ya | Tidak |
|---|---|---|
| `shouldRotate()` — bisa diperbaiki? | lanjut | berhenti, error ke klien |
| Rotasi otomatis aktif? | pindah node baru | cek berikutnya |
| `ipTiedFailure()` — melekat pada IP? | berhenti, error | retry node yang sama |

### Kenapa percobaan pertama ada di dalam loop

Ini perbaikan penting. Sebelumnya percobaan pertama ada **di luar** loop. Satu
node yang menggantung memakan seluruh jatah 45 detik, lalu loop keluar **tanpa
pernah mencoba node lain**.

Terukur: request gagal tepat di detik ke-45 dengan
`http2: timeout awaiting response headers` — persis nilai `ResponseHeaderTimeout`
saat itu. Sekarang semua percobaan ada di dalam loop.

---

## 4. Kapan rotasi dipicu

`shouldRotate()` menggabungkan empat kondisi. Ini bukan sekadar "kalau 429",
karena tiga kondisi lain juga diperbaiki oleh node berbeda.

| Kondisi | Pemicu | Kenapa rotasi menolong |
|---|---|---|
| Rate limit | `429`, `rate limit`, `quota`, `resource_exhausted` | limit melekat pada IP |
| Blokir region | `403` + `region` / `RegionError` | node di negara lain tidak diblokir |
| Koneksi putus | status `0`, `unexpected EOF`, `connection reset` | node tertentu yang rusak |
| Upstream penuh | `503` / `502`, `service_overloaded` | node lain menuju jalur berbeda |

### Aturan yang menjaga klasifikasi tetap sempit

`isTransportFailure` sengaja **tidak** mencocokkan kata `"eof"` sebagai substring.
Sebelumnya `"eof"` cocok di dalam pesan validasi apa pun, sehingga error `400`
mislabel sebagai koneksi putus. Sekarang pencocokan berbasis token (`hasWord`),
dan status `>= 400` tidak pernah dianggap kegagalan transport — server sudah
merespons.

Ini bukan teori: tes regresi menangkapnya dan gagal sebelum diperbaiki.

---

## 5. Cara memilih node

`leafNodes()` menyaring daftar group sebelum memilih. Empat jenis entri dibuang:

1. `DIRECT` / `REJECT` — bukan server upstream
2. Referensi ke group lain — pilihan ditentukan group itu sendiri
3. **Baris pemberitahuan langganan** — lihat bagian 7
4. Node tanpa delay positif — health check belum berhasil

Sisanya diurutkan **dari latency terkecil**, sehingga rotasi cenderung mendarat
di node yang responsif.

Pemilihan node berikutnya berjalan maju dari node sekarang, bukan acak:

```
nodes = [A, B, C, D]        (terurut latency)
now   = B
next  = C
```

Tujuannya agar rotasi berturut-turut menjelajahi daftar, bukan memantul antara
dua entri.

### Router TIDAK memverifikasi IP setelah pindah

Ini keputusan sadar. Versi awal memverifikasi egress IP setelah setiap pindah
node, supaya tidak mendarat di node yang berbagi IP dengan node sebelumnya.

Masalahnya: verifikasi itu butuh satu tunggu (600 ms) plus satu HTTP call
eksternal **per kandidat**, sampai 12 kandidat. Satu rotasi bisa puluhan detik,
padahal rotasi terjadi di tengah request klien — setiap milidetik dirasakan
pengguna.

Sekarang rotasi hanya: pindah node, buang koneksi lama, kembali. Verifikasi
sebenarnya adalah **request ulang ke upstream**. Kalau node baru masih kena
limit, router rotasi lagi. Ini menghilangkan kerja ganda.

---

## 6. Masalah koneksi yang sudah mati

Keep-alive **dimatikan** pada transport proxy. Alasannya spesifik:

Koneksi yang di-pool terikat pada node yang aktif saat koneksi dibuka. Saat
router pindah node, koneksi lama masih menuju node sebelumnya. Memakainya ulang
gagal dengan `unexpected EOF`.

Terukur: **7 dari 8 gagal** dengan keep-alive, **6 dari 6 berhasil** tanpa.

Biayanya kecil karena handshake hanya ke `localhost` (mihomo lokal), bukan ke
upstream.

`ResetConnections()` tetap dipanggil setiap rotasi untuk membuang koneksi sisa.

### Timeout transport

Transport dibangun dari `http.DefaultTransport.Clone()`, **bukan**
`&http.Transport{}` polos. Perbedaannya penting:

| | `&http.Transport{}` | hasil clone |
|---|---|---|
| `DialContext` timeout | **0 (tak terbatas)** | 15 detik |
| `TLSHandshakeTimeout` | **0 (tak terbatas)** | 15 detik |

Dengan literal polos, node yang menerima TCP lalu menggantung di TLS handshake
akan menahan request sampai deadline pemanggil. Terukur: **gagal 65 detik**
dengan status `0` (`EOF`) sebelum diperbaiki.

`ResponseHeaderTimeout` diset **20 detik** — sengaja jauh di bawah jatah retry,
supaya satu node yang tidak menjawab gagal cukup cepat dan masih menyisakan
kesempatan untuk percobaan berikutnya. Pada nilai 45 detik, satu node macet
menghabiskan seluruh jatah.

---

## 7. Mendeteksi stall yang tidak terlihat

`Stream()` kembali begitu **header** tiba, bukan saat ada konten. Jadi node yang
membalas `200` lalu tidak mengirim event apa pun akan dianggap sukses dan klien
menggantung selamanya.

Terukur: request yang hanya menghasilkan **17 token memakan 79 detik**.

Solusinya menunggu **event pertama** maksimal 30 detik (`streamFirst`). Kalau
tidak ada, itu diperlakukan sebagai kegagalan biasa dan dirotasi.

Event pertama tidak hilang: `replayFirst()` mengirimnya kembali di depan stream
sebelum melanjutkan sisanya, jadi klien tetap menerima stream utuh.

### Konteks per percobaan

Setiap percobaan memakai `context.WithCancel`, **bukan** `WithTimeout`.
Konteks harus hidup selama stream dibaca, dan deadline di sini akan memotong
generasi panjang yang sehat. Stall ditangkap oleh `ResponseHeaderTimeout` dan
`firstEventWait`, yang keduanya tidak membatasi panjang respons.

`detachCancel()` membatalkan konteks setelah klien selesai membaca, supaya
koneksi tidak bocor tanpa memotong respons.

### Catatan: dua transport, dua timeout

Ada dua transport terpisah, dan timeout-nya sengaja berbeda:

| Transport | Dipakai untuk | Dial | TLS | Header |
|---|---|---|---|---|
| `ProxyClient()` | request inference | 15 s | 15 s | 20 s |
| `EgressIP()` | cek IP saja | 10 s | 10 s | 20 s |

`EgressIP` lebih pendek karena hanya mengambil satu baris teks, bukan stream
generasi panjang. Transport-nya juga dibuang setiap selesai
(`CloseIdleConnections`) sebab cek IP jarang dipanggil dan tidak perlu memelihara
pool.

---

## 8. Langganan habis ≠ proxy rusak

Saat kuota langganan habis, penyedia mengganti seluruh daftar node dengan
**baris pemberitahuan**:

```
剩余流量：0                    sisa kuota: 0
距离下次重置剩余：16 天         16 hari lagi reset
套餐到期：2026-09-21            kedaluwarsa hari ini
苏菲家宽官网地址：sufe.us/ai.sufe.pro
```

Baris-baris itu terdaftar sebagai entri mirip node Vless dan **semuanya gagal**.
Rotasi tidak menolong karena tidak ada tujuan yang hidup.

Dulu gejalanya tampil sebagai `proxy unreachable: ... EOF`, yang menyesatkan —
terlihat seperti bug jaringan padahal solusinya memperpanjang langganan. Sekarang
router memeriksa baris pemberitahuan dan melaporkan:

```
langganan proxy habis atau kedaluwarsa: 剩余流量：0
```

Status API menyertakan `quota_exhausted: true`.

### Cek egress IP tidak bergantung satu layanan

Dulu hanya `api.ipify.org`. Sebagian node memblokir domain itu, sehingga cek
gagal **walaupun semua request nyata lewat proxy itu berhasil** — inilah yang
membuat error terlihat membingungkan. Sekarang tiga endpoint dicoba bergantian:
`api.ipify.org`, `icanhazip.com`, `ifconfig.me/ip`.

---

## 9. Jatah dan batas

| Parameter | Nilai | Alasan |
|---|---|---|
| Percobaan maksimum | 4 | langganan bisa punya banyak node ber-IP sama |
| Jatah waktu retry | 90 detik | batas atas agar klien tidak menunggu tanpa akhir |
| Tunggu event pertama | 30 detik | menangkap stall yang tidak terlihat |
| Header timeout | 20 detik | gagal cepat, sisakan ruang untuk percobaan lain |
| Dial timeout | 15 detik | hindari TCP menggantung |
| TLS handshake timeout | 15 detik | hindari TLS menggantung |
| Tunggu pindah node | 0 | verifikasi diserahkan ke request ulang |

### Retry dan rotasi adalah dua hal berbeda

Toggle **Rotasi otomatis** (`mihomo_rotate_on_limit`) hanya mengatur
**perpindahan node**. Retry tetap berjalan walau rotasi dimatikan.

Ini hasil dua perbaikan berturut-turut:

1. **Rotasi diabaikan di jalur Zen.** Jalur Zen tidak membaca toggle sama sekali,
   sehingga mematikan rotasi tampak berhasil tapi node tetap berpindah setiap
   kena rate limit.
2. **Lalu retry ikut mati.** Perbaikan pertama membuat rotasi mati berarti
   *tidak ada percobaan ulang sama sekali*, sehingga satu stall sesaat langsung
   gagal — terukur `502` setelah header timeout 22 detik di jalur yang hanya
   punya satu node.

Pemisahannya memakai `ipTiedFailure()`:

| Kegagalan | Melekat pada IP? | Rotasi mati → |
|---|---|---|
| Rate limit (`429`) | ya | berhenti, request ulang ke node sama sia-sia |
| Blokir region (`403`) | ya | berhenti, sama |
| Koneksi putus (`EOF`) | **tidak** | **retry node yang sama** |
| Upstream penuh (`503`) | **tidak** | **retry node yang sama** |

Alasannya: rate limit ditentukan dari alamat pengirim, jadi mengulang dari node
yang sama pasti gagal lagi. Koneksi putus bersifat sesaat, dan node yang sama
sering melayani percobaan berikutnya dengan baik.

Hasil terverifikasi dengan rotasi **dimatikan**: **6/6 request berhasil**,
rata-rata 3,4 detik, node tetap `WARP` sepanjang pengujian.

---

## 10. Hasil terukur

Perjalanan perbaikan, diukur dengan 8 request identik:

| Kondisi | Berhasil | Rata-rata latensi |
|---|---|---|
| Sebelum perbaikan retry | 1/8 | 42.770 ms |
| Setelah retry + transport | 8/8 | 3.486 ms |

Uji ketahanan berikutnya, 15 request (rotasi aktif, langganan berbayar):

```
15/15 berhasil, median ~3.000 ms
```

Jalur gratis WARP, 10 request (rotasi aktif):

```
10/10 berhasil, rata-rata 5.284 ms
```

Jalur gratis WARP, 6 request (rotasi **dimatikan**, retry aktif):

```
6/6 berhasil, rata-rata 3.398 ms, node tetap WARP
```

**Catatan jujur:** latensi masih bervariasi (2–60 detik). Nilai tinggi itu
**retry yang berhasil memulihkan diri**, bukan hang. Penyebabnya kualitas node —
node yang sama bisa 2 detik lalu 46 detik pada request berbeda. Angka terburuk
yang tercatat adalah 79 detik sebelum deteksi stall dipasang.

---

## 11. Kondisi saat ini

```
mihomo_enabled          = 1
mihomo_controller       = http://127.0.0.1:9091
mihomo_proxy            = http://127.0.0.1:7891
mihomo_group            = GRATIS
mihomo_rotate_on_limit  = 0
```

Langganan berbayar (`苏菲家宽`, 68 node) **habis kuota dan kedaluwarsa**, jadi
router diarahkan ke jalur gratis: Cloudflare WARP.

| Komponen | Berkas |
|---|---|
| Config mihomo WARP | `~/.config/mihomo-warp/config.yaml` |
| Launcher | `~/.local/bin/mihomo-warp` |
| Service | `~/.config/systemd/user/mihomo-warp.service` |

WARP terhubung sebagai proxy HTTP di `127.0.0.1:40000` (mode `WarpProxy`), IP
egress `104.28.222.43` (Singapore). Config mihomo utama
(`/etc/mihomo/config.yaml`) **tidak disentuh** karena milik root dan berisi
langganan berbayar.

> **WARP bukan pengganti langganan.** IP-nya bersama dengan pool kecil, dan
> rotasi tidak menolong karena hanya ada satu node. Ini jalur darurat.
> Rotasi otomatis sengaja dimatikan untuk konfigurasi ini.

---

## 12. Peta berkas

| Berkas | Isi |
|---|---|
| `internal/server/mihomo.go` | loop retry, `shouldRotate`, `ipTiedFailure`, `rotateMihomoNode`, `leafNodes`, `streamFirst`, `quotaNotice` |
| `internal/server/proxies.go` | relay Cloudflare, `isRateLimited`, rotasi relay |
| `internal/mihomo/mihomo.go` | klien API kontrol, `ProxyClient`, `EgressIP`, `Healthy()`, `egressProbes` |
| `internal/server/gate.go` | `prepareGate` — mengarahkan prober ke jalur proxy |
| `internal/gate/gate.go` | prober; rotasi pada `429` dan koneksi putus |

### Tes regresi

| Berkas | Yang dijaga |
|---|---|
| `internal/server/mihomo_test.go` | klasifikasi kegagalan; `"eof"` tidak cocok sebagai substring |
| `internal/server/retry_test.go` | retry terpisah dari rotasi (`ipTiedFailure`) |
| `internal/server/quota_test.go` | deteksi langganan habis, tanpa salah lapor node sehat |
| `internal/server/stall_test.go` | deteksi stall; event pertama tidak hilang |
| `internal/gate/prober_test.go` | prober rotasi pada `429`, tidak rotasi pada `403` |

### Endpoint terkait

| Method | Path | Fungsi |
|---|---|---|
| `GET` `PUT` | `/api/mihomo` | Status / simpan pengaturan |
| `POST` | `/api/mihomo/rotate` | Pindah ke node berikutnya |
| `POST` | `/api/mihomo/select` | Pilih node tertentu |
| `POST` | `/api/mihomo/test` | Tes proxy ke Zen |

---

## 13. Hal yang perlu diingat

1. **Rotasi mengganti IP, bukan kredensial.** Kalau limitnya per akun, rotasi
   tidak menolong.
2. **Node bisa berbagi satu IP.** Karena itu ada batas 4 percobaan — langganan
   dengan semua node ber-IP sama tidak akan tertolong rotasi.
3. **Prober gate ikut lewat proxy.** Kalau tidak, probe mengenai IP langsung
   yang kuotanya habis dan melaporkan `baseline rejected with 429` — seolah gate
   menolak request yang sebenarnya tidak pernah sampai.
4. **Relay Cloudflare bukan solusi untuk Zen.** IP-nya jenuh dan dipakai bersama.
5. **Langganan habis terlihat seperti proxy rusak** kalau tidak diperiksa baris
   pemberitahuannya.
