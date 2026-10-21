# ipfarm — C++ proxy farm + IP selector/rotator untuk mot-router

Otomasi penuh pasokan proxy + rotasi IP untuk `mot-router`, ditulis C++ supaya bisa
memeriksa ratusan ribu kandidat paralel dan memilih node tanpa biaya HTTP per rotasi.

## Kenapa

Zen free tier membatasi **per IP**, jadi satu-satunya cara tetap melayani adalah mengganti
IP egress. Rotasi di Go (`internal/server/mihomo.go`) memanggil `AllProxies()` tiap kali =
`GET /proxies` + `GET /providers/proxies`, dan `internal/mihomo/mihomo.go:147` membaca
maksimum 32 MiB. Dengan 100k node di satu provider, JSON melebihi cap → `AllProxies` diam-diam
kembali ke `/proxies`, `leafNodes` kosong, rotasi gagal. `ipfarm` menutup itu: farm di luar
proses Go, node di provider ter-shard, pemilihan skor di memori.

## Subcommand

```bash
make -C tools/ipfarm           # butuh /usr/bin/g++-10 + libcurl (tidak ada `g++` di mesin ini)

./ipfarm fetch                 # unduh 17 sumber -> out/candidates.tsv (dedup proto+ip:port)
./ipfarm check                 # stage1 liveness + stage2 Zen /responses + exit-IP -> out/zen_ok.tsv, state.tsv
./ipfarm emit                  # -> <mihomo-home>/providers/free-pool-XX.yaml + index.txt + state.tsv (nama final)
./ipfarm rotate                # pilih node skor terbaik dgn exit-IP berbeda, PUT /proxies/{group}
./ipfarm run                   # fetch -> check -> emit -> render config -> reload -> wire /api/mihomo
./ipfarm stats                 # ringkasan state.tsv
```

Flag: `--work /tmp/ipfarm`, `--mihomo-home /tmp/mihomo-farm`, `--mihomo http://127.0.0.1:9090`,
`--mihomo-secret <s>` (env `MIHOMO_SECRET`), `--router http://127.0.0.1:8787`, `--group MOT`,
`--conc1 8000`, `--conc2 1500`, `--max-total 4000`, `--shard 2000`, `--interval 1800`.

## Bentuk request yang WAJIB (kalau tidak, upstream 403)

Free tier menyaring berdasarkan **bentuk**, bukan kredensial. `src/check.cpp` mengirim persis:

- `User-Agent: opencode/1.18.31`
- `x-opencode-client: cli`, `x-opencode-project: global`
- `x-opencode-session: ses_<12 hex><14 alnum>` — **wajib**; menghilangkannya = `403`
- lima tool wajib di body: `bash`, `edit`, `glob`, `grep`, `read` (`zen.RequiredTools`)
- `stream:true`, `max_output_tokens:16` (nilai <16 ditolak `invalid_request_error`)
- endpoint: `POST /zen/v1/responses` (bukan `/chat/completions`)

## Ukuran terukur (mesin ini: 2 core, RAM 3.6 GiB, libcurl 7.68)

| Tahap | Hasil |
|---|---|
| fetch 17 sumber | 252.943 baris mentah → **123.983 kandidat uniq** (57.655 IP unik) |
| check (conc1 900, conc2 300) | stage1 live **283**; Zen terima **11**; 8 punya exit-IP |
| emit | 1 shard × 11 node |
| `curl /providers/proxies \| wc -c` | 6.886 byte (jauh di bawah cap 32 MiB) |

Yield kecil itu wajar: proxy gratis dipakai bersama, jadi mayoritas `429`/mati. Naikkan
`--max-total` dan tambah sumber di `sources.txt` untuk memperbesar pasokan.

## Catatan operasional

- **Proxy mati itu normal.** Bahkan 20 koneksi paralel ke satu proxy "sehat" sering timeout;
  itu kapasitas proxy gratis, bukan bug runner. Jangan turunkan timeout untuk "memperbaiki".
- **`CURLOPT_CONNECT_ONLY` tidak dipakai.** Di libcurl 7.68 pada mesin ini, proxy yang
  `curl` biasa bawa request `200` justru balas "Timeout was reached" di mode connect-only.
  Stage 1 memakai GET biasa; jangan kembali ke connect-only.
- **stop_after_bytes tanpa proxy aman, dengan proxy tidak.** Batas 1 byte di stage 2 terbukti
  membunuh transfer sebelum header (status 0). Stage 1 karena itu membaca body penuh dari
  `/zen/v1/models` (kecil).
- **state.tsv berpindah kunci.** `check` menulis baris ber-key `host:port`; `emit` menulis
  ulang dengan nama node mihomo (`pXX-NNNN-<ms>ms`) sambil mempertahankan `ok`/`fail`.
  `rotate` selalu memakai nama node.
- **`SIGPIPE` di-ignore** — proxy gratis sering menutup koneksi di tengah request.
- **`store-selected: true`**: nama node memuat `zen_ms`, jadi nama berubah tiap siklus
  `run`; pilihan tersimpan bisa menunjuk nama lama. Kalau `GET /api/mihomo` menunjukkan
  `node` kosong setelah reload, set `profile.store-selected: false` di
  `mihomo-config.tmpl` lalu muat ulang.

## Verifikasi

```bash
./ipfarm fetch && ./ipfarm check && ./ipfarm emit
mihomo -d /tmp/mihomo-farm -f /tmp/mihomo-farm/config.yaml -t   # exit 0
./ipfarm rotate && ./ipfarm rotate                              # node + exit_ip berbeda
curl -s http://127.0.0.1:8787/api/mihomo        # reachable:true, groups[].nodes > 0
curl -s -X POST http://127.0.0.1:8787/api/mihomo/rotate          # ok:true
curl -s -X POST http://127.0.0.1:8787/api/mihomo/test            # ok:true, status 200
```

Terakhir dijalankan di mesin ini: `/api/mihomo` → `reachable:true`, `groups[0]` `MOT` nodes 7;
`/api/mihomo/rotate` → `ip_changed:true`; `/api/mihomo/test` → `ok:true status:200`;
inference `POST /v1/chat/completions` → HTTP 200 + SSE valid, log request mencatat
`relay=mihomo:MOT`.