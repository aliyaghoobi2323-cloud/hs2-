# حلقه‌های کنترلی و نگهبان‌ها — مسیر mtcp / l3mtcp (و جداگانه dgtun)

> **دامنه:** همهٔ حلقه‌های بسته (اندازه‌گیری ← تصمیم ← عمل)، نگهبان‌ها و زمان‌سنج‌هایی که رفتار تونل را در زمان اجرا تغییر می‌دهند. تمرکز روی تونل اصلی کاربر یعنی `l3mtcp` است: پول لینک mtcp (TLS + smux) برای پورت‌های کاربر، و کانال جانبی `hs0` روی جریان `kindL3` هر لینک. dgtun در بخش ۳ جداگانه آمده است.
>
> **ثبت پایه:** `0812bc9`. همهٔ مسیرها نسبت به `/home/user/hs2-/hs2-src` هستند، مگر خلافش گفته شود. هیچ فایلی در مخزن تغییر نکرد (`git status` تمیز ماند).
>
> **فایل‌هایی که برای این سند کامل خوانده شد:** `engine/linkmanager.go`، `engine/autopilot.go`، `engine/health.go`، `engine/health_linux.go`، `engine/stuck.go`، `engine/loss.go`، `engine/refill.go`، `engine/dialgate.go`، `engine/wedge.go`، `engine/mempressure.go`، `engine/control.go`، `engine/stats.go`، `engine/stream.go`، `engine/mtcp_link.go`، `engine/l3_link.go`، `engine/stream_iran.go`، `engine/stream_reverse.go`، `engine/stream_kharej.go`، `engine/exit_pool.go`، `cmd/hs2/hostcpu.go`، `tlscarrier/tune_linux.go`. بخش‌های مرتبط این‌ها هم خوانده شد: `engine/peerinfo.go` (`openInfo`)، `engine/engine.go` (`acceptBackoff`)، `engine/burstlog.go`، `engine/exitstats.go`، `engine/dgpool.go` (حلقه‌ها، `add`، `pick`، `muteTick`، `sampleHealth`، `reconcile`، `drainTick`)، `cmd/hs2/status.go` (نویسندهٔ وضعیت، فایل گرم، `tcpMemWatch`، `cpuMeter`)، `cmd/hs2/main.go` (`applyTuning`، `warmLinks`، `setMemoryLimit`، `drainIdle`)، `tlscarrier/carrier.go` و `tlscarrier/server.go` (مهلت‌ها)، smux v1.5.24 (`session.go`: keepalive، `recvLoop`، `OpenStream`)، `udpcarrier/{carrier,rate,pacer,governor}.go` (ثابت‌ها و قاعدهٔ stage)، و آزمون‌های `wedge_test.go`، `mempressure_test.go`، `stuck_test.go` (فهرست زیرآزمون‌ها).
>
> **نقشه‌های پیشین** `04-linkmanager.md`، `05-autopilot-health.md`، `08-dgtun.md` و `09-udp-fec.md` فقط راهنما بودند. هر عدد و هر path:line این سند دوباره با کد تطبیق داده شد، مگر جاهایی که صریحاً نوشته‌ام «طبق نقشهٔ ۰۸/۰۹».
>
> **آزمون‌ها:** `go test -short -count=1 -run 'Wedge|Stuck|Loss|Memory|Refill|DialGate|Autopilot|StreamL3|Pick|Control|Degrade|Reverse|Drain|Reap|Heal|Reconcile|Relay' ./engine/` ⇒ `ok … 39.399s` (go در `/usr/local/go/bin`).
>
> **قرارداد زمان‌ها:** «زمان واکنش» از لحظهٔ شروع رخداد تا اولین عمل حلقه است. عددهایی که با «محاسبه» آمده‌اند را خودم از کد درآورده‌ام و اندازه‌گیری نشده‌اند. عددهایی که با «اندازه‌گیری» آمده‌اند از `CHANGELOG.md` (ریشهٔ مخزن) نقل شده‌اند.

---

## ۰. خلاصهٔ فشرده

1. **ساعت مشترکی وجود ندارد.** در l3mtcp دست‌کم هفت ساعت مستقل و بی‌هم‌فاز کار می‌کنند:
   - تیک پول ۲ ثانیه‌ای فقط در لبه (`engine/health.go:18`، `engine/linkmanager.go:567-581`)؛
   - تیک نگهبان wedge، ۲ ثانیه‌ای و یکی برای کل پردازه، در هر دو سرور (`engine/wedge.go:62`، `220-226`)؛
   - تیک نویسندهٔ وضعیت، ۲ ثانیه‌ای، در هر دو سرور (`cmd/hs2/status.go:29`، `335-347`)؛
   - کانال کنترل هر لینک: ۳ ثانیه، یا ۹ تا ۱۵ ثانیه برای لینک بی‌کار (`engine/control.go:102-130`)؛
   - keepalive smux هر نشست: ۴ تا ۸ ثانیه، با بررسی ۲۴ ثانیه‌ای (`engine/mtcp_link.go:278-279`)؛
   - ناظر نشستِ کانال L3: ۲ ثانیه (`engine/l3_link.go:143-161`)؛
   - refill: ۱۰۰ میلی‌ثانیه (`engine/refill.go:56`).
2. **همهٔ تصمیم‌های سطح پول فقط در لبه (ایران) گرفته می‌شوند**، چه مستقیم چه معکوس. خروجی (خارج) حلقهٔ سلامت لینک ندارد. سمت خروجی فقط این‌ها هست:
   - نگهبان wedge؛
   - keepalive smux؛
   - `TCP_USER_TIMEOUT`؛
   - ناظر L3؛
   - در حالت معکوس، پول slot که از لبه فرمان می‌گیرد (`engine/exit_pool.go:14-30`).
3. **نردبان تشخیص خرابی یک لینک** (از سریع به کند؛ همه در بخش ۲ با جزئیات):
   - wedge guard: ≈۴ تا ۸ ثانیه، و فقط رله‌های گیر را می‌کشد؛
   - stuck: ≈۸ تا ۱۳ ثانیه، و فقط اگر لینک سالم دیگری «شاهد» باشد؛
   - suspect: ۱۲ تا ۱۴ ثانیه پس از آخرین بایت دریافتی؛
   - ناظر L3: ۱۲ تا ۱۴ ثانیه؛
   - `TCP_USER_TIMEOUT`: ۲۰ ثانیه دادهٔ تأییدنشده؛
   - keepalive smux: ۲۴ تا ۴۸ ثانیه؛
   - loss: سه نمونه، یعنی حدود ۴ تا ۶ ثانیه برای آپلود و ۹ ثانیه یا بیشتر برای دانلود.
4. **فقط مرگ نشست یا سوکت یک لینک را می‌کشد.** موارد دیگر فقط لینک را کنار می‌گذارند:
   - suspect، retiring و `pressed` برگشت‌پذیرند؛
   - degraded برگشت‌ناپذیر است و لینک پس از حداکثر ۹۰ ثانیه بسته می‌شود.
5. **شکاف‌های مهم** (با شاهد کد در بخش‌های ۴ و ۵):
   - (الف) خوانندهٔ کند ولی زنده‌ای که bucket نشست را خالی نگه می‌دارد را هیچ نگهبانی نمی‌گیرد. چنین لینکی «بی‌فشار» و «سبک» دیده می‌شود و کاربر تازه هم جذب می‌کند.
   - (ب) لینکی که پراتلاف است ولی ترافیکش کم است (زیر ۹۶KiB در تیک) را کسی قضاوت نمی‌کند.
   - (ج) لینکی با تأخیر ۲ تا ۵ ثانیه را هیچ قاعده‌ای نمی‌گیرد. pick و autopilot اصلاً RTT نمی‌بینند.
   - (د) جریان‌های TUN از degraded، retiring و suspect خبر ندارند و فقط با مرگ کانال L3 جابه‌جا می‌شوند.
   - (هـ) کانال L3 که روی یک لینک زنده رها شود، دیگر هرگز باز نمی‌شود.
   - (و) `OpenStream` و نوشتن سرآیند جریان مهلت جداگانه ندارند: SYN تا ۳۰ ثانیه، و سرآیند تا مرگ لینک.
   - (ز) در l3mtcp، اشباع CPU سرور هیچ واکنشی ندارد.
   - (ح) dial مستقیم backoff نمایی ندارد.
6. **تعامل‌های مهم** (بخش ۴):
   - suspect و degraded باعث می‌شوند S کمتر از T شود. این رشد autopilot را می‌بندد و ممکن است پروب را abort کند (با backoff بین ۶۰ ثانیه و ۸ دقیقه).
   - فشار حافظه همهٔ حکم‌ها را خاموش می‌کند و نگهبان را تهاجمی می‌کند.
   - wedge لینک را از قاعدهٔ stuck معاف می‌کند، ولی اگر wedge آزاد نشود هیچ‌کس جایش را نمی‌گیرد.
   - loss و stuck سقف تخلیهٔ جدا دارند (`engine/loss.go:209` و `engine/linkmanager.go:1981`)، پس در یک تیک ممکن است تا دو برابر سقف لینک تخلیه شود.
   - pick لینکی را که تازه مرده یا گیر کرده «سبک‌ترین» می‌بیند.

---

## ۱. ساعت‌ها و ترتیب اجرا در l3mtcp

```
EDGE (Iran)                                           EXIT (Kharej)
──────────────────────────────────────────────        ─────────────────────────────────────────
LinkManager.Run (direct) every 2 s                    (no pool tick in direct; reverse: exitPool slots,
  reap → sampleHealth → heal → autoscale               event-driven + backoff 0.5–8 s)
  (decideTarget+reconcile) → drainTick → publishStats
LinkManager.runAccept (reverse) every 2 s
  sampleHealth → reconcile(decideTarget) → drainTick
  → sweepReverse → publishStats
queueDial goroutines ── dialGate (≤8 in flight,      exitPool.runSlot ── same dialGate (process-wide)
  40–160 ms between starts)
refill episode: 100 ms ticks, ≤10 s
per link: openControl 3 s / 2–4 s / 9–15 s            serveControl (answers)
          openStats (poll on ≥16 KiB/tick)            serveStats (record + mem-pressure flag)
          openInfo (≤3 tries ×10 s, then 1/min)       serveInfo
          openPoolCtl (reverse: 3 s fast / 30 s)      servePoolCtl → exitPool.setTarget
          openL3: writeLoop (keepalive 2 s|~10 s),    kindL3: same l3Link machinery
                  watchSession every 2 s (12 s)
per session: smux keepalive NOP 4–8 s, check 24 s     same
             watchConn: first I/O error ⇒ close        same
             TCP_USER_TIMEOUT 20 s (kernel)            same
process: guards.run every 2 s (all sessions)          same
daemon:  status writer every 2 s → tcpMemWatch        same (exit's flag goes to the edge in
         → engine.SetTCPMemPressure; hostMeter →        kindStats records)
         udpcarrier.SetHostSaturated (dgtun only)
```

- ترتیب تیک مستقیم در `engine/linkmanager.go:575-580` و ترتیب تیک معکوس در `engine/linkmanager.go:1133-1137` است.
- **نکتهٔ ترتیب:** حکم‌های loss و stuck که در `sampleHealth` صادر می‌شوند، در **همان تیک** به `heal` (مستقیم) یا `sweepReverse` (معکوس) می‌رسند. سپس autopilot با `apSample` همین تیک تصمیم می‌گیرد. چون نمونه پیش از `heal` ساخته شده، لینکی که در این تیک degraded شده در نمونه دیگر serving نیست (`apLink.serving = ml.serving()`، `engine/linkmanager.go:2066-2073`). ولی جایگزینی که `heal` تازه صف کرده هنوز در نمونه نیست.
- در حالت معکوس، `reap` و `heal` صدا زده نمی‌شوند. کارشان را `sweepReverse` (`engine/linkmanager.go:1145-1210`) و `DropLink` (`engine/linkmanager.go:474-496`) انجام می‌دهند. `DropLink` به‌محض بسته‌شدن نشست، از `acceptReverseLinks` صدا زده می‌شود (`engine/stream_reverse.go:97`).

---

## ۲. کاتالوگ حلقه‌ها در مسیر mtcp / l3mtcp

### ۲.۰ جدول خلاصه

| ID | حلقه | نوع | دوره | واکنش (کمینه – بیشینه) | محل اصلی |
|---|---|---|---|---|---|
| `L01` | تیک پول (ارکستراتور) | زمان‌بند | ۲s | — | `engine/linkmanager.go:546-583`، `1124-1140` |
| `L02` | autopilot: FLOOR | تصمیم اندازه | ۲s | ≈۸–۱۶s (محاسبه) | `engine/autopilot.go:503-514` |
| `L03` | autopilot: GROW و داوری پروب | تصمیم اندازه | ۲s | شروع ≈۶–۱۰s پس از فشار؛ حکم ۱۴–۳۴s پس از مسلح‌شدن | `engine/autopilot.go:548-579`، `618-780` |
| `L04` | autopilot: CONFIRM و سقف مسیر | ضد خزش | ۲s | پنجرهٔ ۶۰s | `engine/autopilot.go:470-500`، `755-767`، `800-811` |
| `L05` | autopilot: SHRINK | تصمیم اندازه | ۲s | گام اول ≈۱۲۰s پس از افت تقاضا، سپس هر ≥۳۰s | `engine/autopilot.go:582-603` |
| `L06` | autopilot: RESTORE و hold | ضد نوسان | ۲s | همان تیک | `engine/autopilot.go:522-543` |
| `L07` | autopilot: backoff، abort و بازنشانی k | زمان‌بندی پروب | ۲s | ۳۰s تا ۸m | `engine/autopilot.go:448-461`، `626-643`، `784-791` |
| `L08` | تخمین ظرفیت هر لینک | ورودی اندازه | ۲s | پنجرهٔ ۳۰m | `engine/autopilot.go:850-890` |
| `L09` | `reconcile` (محرک) | عمل | ۲s | همان تیک (dial از دروازه) | `engine/linkmanager.go:767-862` |
| `L10` | `drainTick` (بستن retiring و بازپس‌گیری بی‌کار) | عمل | ۲s | بستن خالی‌ها ۲–۸ در تیک؛ بی‌کار ۳۱۰s؛ اجبار ۲۰m | `engine/linkmanager.go:959-1117` |
| `L11` | شروع گرم و فایل warm | حافظهٔ بین ری‌استارت | ۲s (نوشتن) | خواندن یک‌باره | `cmd/hs2/status.go:192-246`، `cmd/hs2/main.go:280-293` |
| `L12` | `Pick` / `pickKey` و کلاهک انفجار | جای‌گذاری | هر اتصال | فوری | `engine/linkmanager.go:1470-1586` |
| `L13` | فشار آپلود | حسگر | ۲s | ۲–۶s | `engine/linkmanager.go:1797-1806` |
| `L14` | فشار دانلود (kindStats) | حسگر | با poll | ≈۴–۶s؛ کهنگی ۶s | `engine/linkmanager.go:1801-1814`، `2032-2064`، `engine/stats.go:106-249` |
| `L15` | refill hold | جای‌گذاری پس از قطعی | ۱۰۰ms | ≤۱۰s | `engine/refill.go:94-351` |
| `L16` | دروازهٔ info | جای‌گذاری | یک‌باره | ≤۵s (یا تا ۳۰s اگر SYN گیر کند) | `engine/peerinfo.go:267-293`، `engine/linkmanager.go:1554` |
| `L17` | suspect | سلامت (برگشت‌پذیر) | ۲s | ۱۲–۱۴s پس از آخرین بایت | `engine/linkmanager.go:1735-1744`، `317` |
| `L18` | کانال کنترل (ping/pong) | حسگر | ۳s / ۲–۴s / ۹–۱۵s | — | `engine/control.go:72-181` |
| `L19` | stuck | سلامت (برگشت‌ناپذیر) | ۲s | ≈۸–۱۳s (محاسبه) | `engine/linkmanager.go:1864-1901`، `1968-1991` |
| `L20` | مسیر کند/شلوغ و پنجرهٔ بهبود | بازدارندهٔ حکم | ۲s | فوری؛ مهار ۳۰s–۲m | `engine/linkmanager.go:1906-1950`، `engine/stuck.go:97-138` |
| `L21` | loss | سلامت (برگشت‌ناپذیر) | ۲s / پنجرهٔ pong | ≈۴–۶s (آپلود)، ≈۹s+ (دانلود) | `engine/linkmanager.go:1816-1860`، `engine/loss.go:98-225` |
| `L22` | تخلیهٔ degraded (`heal` / `sweepReverse`) | عمل | ۲s | همان تیک؛ پله‌های ۴۵s/۱۵s/۹۰s | `engine/linkmanager.go:2089-2182`، `1145-1291`، `1361-1398` |
| `L23` | `reap` / `DropLink` و short-lived | عمل | ۲s / رخداد | ≤۲s / فوری | `engine/linkmanager.go:1408-1462`، `474-496` |
| `L24` | صف dial و دروازه (مستقیم) | عمل | رخداد | ≥۴۰ms بین شروع‌ها؛ connect ≤۸s؛ auth ≤۱۰s | `engine/linkmanager.go:891-950`، `engine/dialgate.go:22-75` |
| `L25` | slotهای پول خروجی (معکوس) | عمل | رخداد | backoff ۰٫۵–۸s؛ scout ≤۲s | `engine/exit_pool.go:41-432` |
| `L26` | pool-control و نگهبان‌های churn (معکوس) | هماهنگی دو سر | ۳s / ۳۰s / تغییر | ≤۱٫۵s برای تغییر | `engine/exit_pool.go:448-584`، `engine/linkmanager.go:409-470`، `959-1018`، `engine/stream_reverse.go:36-101` |
| `L27` | نگهبان wedge | نگهبان رله | ۲s | ≈۴–۸s پس از پارک شدن خواننده | `engine/wedge.go:45-280` |
| `L28` | فشار حافظهٔ TCP هسته | بازدارنده و نگهبان | ۲s | فوری؛ اثر تا ۳۰s–۲m بعد | `cmd/hs2/status.go:831-855`، `engine/mempressure.go:22-36` |
| `L29` | `relayDieGrace` | پایان رله | رخداد | ۵s | `engine/wedge.go:68-70`، `328-339` |
| `L30` | keepalive smux | مرگ نشست | NOP ۴–۸s؛ بررسی ۲۴s | ۲۴–۴۸s | `engine/mtcp_link.go:278-279`؛ smux `session.go:399-422` |
| `L31` | `watchConn` و `TCP_USER_TIMEOUT` | مرگ نشست | پیوسته | ۲۰s (+≤۲s برای reap) | `engine/stream.go:74-123`، `tlscarrier/tune_linux.go:22,46` |
| `L32` | پس‌فشار smux و `TCP_NOTSENT_LOWAT` | کنترل جریان | پیوسته | — | `engine/mtcp_link.go:258-264`، `tlscarrier/tune_linux.go:19,44` |
| `L33` | مهلت‌های SYN/FIN smux و دست‌دهی TLS | نگهبان مهلت | رخداد | ۳۰s / ۱۰s / ۳۰s | smux `session.go:17,524-527`؛ `tlscarrier/server.go:40,47` |
| `L34` | کانال جانبی L3 (صف، کهنگی، keepalive، ناظر نشست) | AQM و سلامت | هر بسته / ۲s | ۶۰ms / ۵s / ۱۲–۱۴s / ۳۰s | `engine/l3_link.go:49-393` |
| `L35` | جریان‌های UDP کاربر | AQM و بی‌کاری | هر datagram / ۳۰s | ۲–۲٫۵m برای بی‌کاری | `engine/stream_iran.go:270-416`، `engine/stream.go:270-300` |
| `L36` | مهلت‌های سمت خروجی (نوع جریان، dial پنل) | نگهبان مهلت | رخداد | ۱۰s / ۵s | `engine/stream_kharej.go:173-244` |
| `L37` | `setMemoryLimit` | ایستا | یک‌باره | — | `cmd/hs2/main.go:305-320` |
| `L38` | tune (sysctl) و `applyTuning` | ایستا | یک‌باره | — | `cmd/hs2/main.go:256-275`؛ `02-cmd-ops-tune.md` |
| `L39` | `cpuMeter` / `hostMeter` | فقط لاگ (در l3mtcp) | ۲s | ۳ نمونه / ۵ نمونه | `cmd/hs2/status.go:351-404`، `cmd/hs2/hostcpu.go:58-242` |
| `L40` | `acceptBackoff` | نگهبان شنونده | رخداد | ۵ms تا ۱s | `engine/engine.go:211-239` |
| `L41` | `burstLog` و محدودکننده‌های لاگ | فقط لاگ | ۱۰s / ۱m / ۱۰m | — | `engine/burstlog.go:17-73` و جاهای دیگر |

### ۲.۱ گروه الف: اندازهٔ پول (autopilot و محرک)

**`L01` — تیک پول**
- **ورودی:** هیچ؛ زمان‌بند است.
- **عمل:** حلقهٔ مستقیم (`engine/linkmanager.go:567-581`) و حلقهٔ معکوس (`engine/linkmanager.go:1125-1139`) هر `healthTick` = 2s (`engine/health.go:18`) زنجیرهٔ کامل را اجرا می‌کنند.
- **پرشدن اولیه** (فقط مستقیم): `min(warm, ceil(warm/4)+gateInflight)` dial بی‌درنگ صف می‌شود (`engine/linkmanager.go:564-566`). بقیه را `reconcile` تیک‌به‌تیک اضافه می‌کند.
- **لاگ:** ندارد.

**`L02` — FLOOR (کف از جریان‌های فعال)**
- **ورودی:** `fl5` = **کمینهٔ** `flowing` در ۵ تیک آخر، شامل تیک فعلی (`engine/autopilot.go:382-387`).
- **تعریف flowing:** جریان کاربری با EWMA (τ=10s) ≥ 2KiB/s که در ۶ ثانیهٔ اخیر بایت داشته، یا جریانی که ≥256B/s در هر ۳ نمونهٔ آخر داشته (`engine/mtcp_link.go:117`، `engine/health.go:41-48`).
- **شرط:** `floor = clamp(ceil(fl5/perLink)) > T`. در `!growable` سقف floor برابر `S+R` است (`engine/autopilot.go:503-507`).
- **عمل:** پروب در جریان لغو می‌شود، `T = floor` و `lastGrowAt = now`. پرش مستقیم است و پروبی در کار نیست (`engine/autopilot.go:507-514`).
- **زمان واکنش (محاسبه):**
  - جریان سنگین در اولین نمونه flowing می‌شود (EWMA پس از یک تیک ≈۰٫۱۸ نرخ). جریان سبک با قاعدهٔ steady پس از ۳ نمونه (۴–۶s).
  - `fl5` باید ۵ تیک پیاپی بالا بماند، یعنی +۸s.
  - پس **≈۸ تا ۱۶ ثانیه** طول می‌کشد. پس از آن dial از دروازه انجام می‌شود (≈۱۰ در ثانیه).
- **لاگ:** `pattern %d → %d links: %d active connections (per_link %d)` (`engine/autopilot.go:513`).
- **نکته:** ترافیک TUN در `flowing` نیست، چون جریان L3 خام است و شمرده نمی‌شود (`engine/mtcp_link.go:183`). پس FLOOR هرگز به خاطر بار hs0 بالا نمی‌رود.

**`L03` — GROW و داوری پروب (`judge`)**
- **ورودی:**
  - S، R و P (serving، retiring و pressed-serving) از `apSample`؛
  - `shortTick = P≥1 && S−P < spare(P)` (`engine/autopilot.go:368`)؛
  - `isShort` = short بودن در ≥۳ تیک از ۵ تیک آخر (`engine/autopilot.go:374-380`)؛
  - U = `clamp(max(fl60+2, p60+spare(p60)))` (`engine/autopilot.go:416`).
- **شرط شروع** (`engine/autopilot.go:548`): همهٔ این‌ها با هم:
  - `isShort` و `shortTick`؛
  - `s.growable`؛
  - `S ≥ T`؛
  - `T < U` و `T < max`؛
  - `now ≥ next`؛
  - تاریخچه ≥ ۱۰ تیک (۲۰s).
- **عمل:**
  - گام برابر `min(ceil(T/4), 32)` است. اگر پروب قبلی ظرف ۶۰s موفق شده باشد (زنجیره)، گام `min(ceil(T/2), 64)` است (`engine/autopilot.go:552-555`).
  - `T = to` و یک `apProbe` ساخته می‌شود. پایهٔ آن میانگین و واریانس G در ۱۰ تیک آخر، و `before[id]` (نرخ `rate10` لینک‌های retiring) است.
- **داوری:**
  - **مسلح شدن:** وقتی `S ≥ to`. اگر بیش از `15s + (to−from)×150ms` بگذرد و مسلح نشود، abort می‌شود (`engine/autopilot.go:622-645`).
  - **نشست:** ۲ تیک.
  - **نگاه‌ها:** در n=5، 10 و 15 تیک ارزیابی، یعنی ۱۴، ۲۴ و ۳۴ ثانیه پس از مسلح شدن (`engine/autopilot.go:646-687`).
- **حکم‌ها** (`engine/autopilot.go:708-778`):

  | حکم | شرط | اثر |
  |---|---|---|
  | موفق | `rNew ≥ rMin && dG ≥ max(0.5·rNew, 2.5·se, 0.05·gb)` | `next=+4s` |
  | relieved | فقط در نگاه ۱۵ | `next=+30s` |
  | شکست | `(rNew≥rMin‖busy)` در n=15، یا زودهنگام در n=10 با `dG<0.25·rNew` | `T=from`، `k++`، `next=+backoff` |
  | بی‌نتیجه | n=15 و هیچ‌کدام از بالا | `T` می‌ماند، `next=+30s` |

- **زمان واکنش (محاسبه):**
  - فشار ۲ از ۳ (۲–۶s)، سپس `isShort` ۳ از ۵ (+۴s)؛ پس شروع پروب ≥۶ تا ۱۰ ثانیه پس از شروع فشار است، به شرط آنکه تاریخچه و `next` اجازه دهند.
  - حکم موفق زودترین ۱۴s پس از مسلح شدن است.
- **لاگ‌ها:**
  - `pattern %d → %d links (probe): …` (`engine/autopilot.go:577`)
  - `… kept: +%.1f Mbit/s …` (`:719`)
  - `… kept as headroom …` (`:732`)
  - `sized to %d links … (path is full); next check in %s` (`:769`)
  - `… kept as spares …` (`:777`)
  - `pattern back to %d links: wanted %d but only %d came up …` (`:641`)

**`L04` — CONFIRM و سقف مسیر (`apCeil`)**
- **CONFIRM:** وقتی پروبی موفق یا relieved باشد و `k>0` یا آخرین شکست کمتر از ۳۰ دقیقه پیش بوده باشد (`engine/autopilot.go:800-811`):
  - به مدت ۶۰s، میانگین G باید ≥ `max(gb + max(0.5·dG, 0.05·gb), gb + 2·se)` بماند؛
  - وگرنه `T=from` و `k=kPrev+1` (`engine/autopilot.go:470-500`)؛
  - در این مدت پروب تازه‌ای شروع نمی‌شود و زنجیره خاموش است.
- **سقف مسیر:** در حکم شکست، اگر سقف معتبری (کمتر از ۱ ساعت) با `from > c.n` و `gb ≤ 1.1·c.g` وجود داشته باشد، `T = c.n` (`engine/autopilot.go:755-767`).
- **لاگ‌ها:**
  - `pattern %d → %d links: the gain after the last probe did not last …` (`:494`)
  - `… %d links carry no more than %d did …` (`:761`)

**`L05` — SHRINK (کوچک‌سازی)**
- **ورودی:** `H` (`engine/autopilot.go:427-442`) =
  `clamp(max(min(max(needSat, needBW), U), holdN, fHold))`، که در آن:
  - `needSat = p60 + spare(p60)`؛
  - `needBW = ceil(gPeak / (0.7·cCap))`؛
  - `fHold = ceil(fl60/perLink)`.
- **شرط** (`engine/autopilot.go:589-590`): همهٔ این‌ها با هم:
  - `H < T` پیوسته به مدت ≥۶۰s؛
  - `!shortIn60 || T > U`؛
  - دست‌کم ۳۰s از آخرین کوچک‌سازی؛
  - دست‌کم ۶۰s از آخرین رشد.
- **عمل:**
  - `T −= ceil((T−H)/2)` با کف H؛
  - `shrinkFrom` ثبت می‌شود؛
  - لینک‌های اضافه فقط retiring می‌شوند (`L09`).
- **زمان واکنش (محاسبه):** H خودش تا ۶۰s پس از افت تقاضا بالا می‌ماند، چون `fl60`، `p60` و `gPeak` روی ۳۰ تیک حساب می‌شوند. پس اولین گام ≈۱۲۰s پس از افت تقاضاست و گام‌های بعدی هر ≥۳۰s. بستن فیزیکی لینک دقیقه‌ها بعد اتفاق می‌افتد (`L10`).
- **لاگ:** `pattern %d → %d links: demand needs ~%d — …; extra links take no new connections and close when theirs end` (`engine/autopilot.go:602`).

**`L06` — RESTORE و hold**
- **شرط:** `isShort && shrinkFrom > T && now − lastShrinkAt ≤ 60s` (`engine/autopilot.go:522`).
- **عمل:**
  - `T = shrinkFrom`. لینک‌های retiring هنوز زنده‌اند، پس `reconcile` بدون dial آن‌ها را برمی‌گرداند.
  - hold برابر `10m << j` با سقف ۲ ساعت، که در آن j تعداد undoها در ۲ ساعت اخیر است (`engine/autopilot.go:526-539`).
  - اگر `gPeak < 0.6·hold.g` شود، hold باطل است (`engine/autopilot.go:424`).
- **لاگ:** `… the shrink to %d left links at their limit — undone, held for %s` (`:542`).

**`L07` — زمان‌بندی پروب: backoff، abort و بازنشانی k**
- **شکست:** `30s << (k−1)`، با سقف ۸ دقیقه و ضریب لرزش در بازهٔ `[0.8, 1.2]` (`engine/autopilot.go:784-791`).
- **abort:** `60s << (aborts−1)`، با سقف ۸ دقیقه و **بدون لرزش** (`engine/autopilot.go:633-638`). مسلح شدن `aborts` را صفر می‌کند.
- **بازنشانی k:** اگر میانگین G در ۲۰ ثانیهٔ آخر > `1.3·fail.g` باشد، یا `fl5 > 1.5·fail.flows + 2`، آنگاه `k=0` و `next ≤ now+30s`؛ وگرنه پس از ۳۰ دقیقه. در طول CONFIRM هیچ‌کدام اعمال نمی‌شود (`engine/autopilot.go:448-461`).

**`L08` — تخمین ظرفیت هر لینک**
- **ورودی:** `sustained` (کمینهٔ ۳ تیک جهت غالب) لینک‌های serving و pressed (`engine/autopilot.go:353-367`).
- **عمل:**
  - برای هر لینک، بهترین مقدار در پنجرهٔ ۳۰ دقیقه نگه داشته می‌شود (`engine/autopilot.go:850-870`).
  - تخمین = میانهٔ این مقادیر، به شرط ≥۶ لینک (`engine/autopilot.go:876-890`).
  - اگر `gPeak ≥ 16KiB/s` باشد، `needBW = ceil(gPeak/(0.7·cCap))` (`engine/autopilot.go:417-421`).
- **اثر:** فقط روی H (`L05`). این مقدار هیچ‌وقت خودش T را بالا نمی‌برد.

**`L09` — `reconcile` (محرک T)**
- **ورودی:** T. دسته‌بندی لینک‌ها: لینک مرده، degraded، draining و suspect **نه serving هستند نه retiring** (`engine/linkmanager.go:771-779`).
- **رشد:** اول retiringها برمی‌گردند؛ به ترتیب بیشترین `open`، سپس تازه‌ترین `lastByte`. `servingSince=now` می‌شود (`engine/linkmanager.go:781-796`).
- **کوچک‌سازی:** قربانی‌ها به ترتیب کمترین `flowing`، سپس `recent`، سپس `open`، سپس `rate10` انتخاب می‌شوند. `retiring=true` و `pressed=false` (`engine/linkmanager.go:797-816`).
- **dial (فقط مستقیم):**
  - `n = T − S − inflight`؛
  - حداکثر `ceil(T/4)`؛
  - حداکثر `dialRoom − inflight`؛
  - حداکثر `ceil(T/4) + 8 − inflight` (`engine/linkmanager.go:844-861`).
  - در `dialRoomLocked`، هر لینکی که draining نباشد «slot» حساب می‌شود، **suspect هم** (`engine/linkmanager.go:2200-2208`).
- **لاگ‌ها:**
  - `link(s) %s back in service …` (`:820`)
  - `link(s) %s retiring — …` (`:823`)
  - `dialed %d link(s) — …` (`:831`)
  - `want %d serving links, only %d up — dials failing …` (هر ۳۰s، `:833-841`)

**`L10` — `drainTick` (بستن retiring و بازپس‌گیری)**
- **شرط بستن یک لینک retiring** (`engine/linkmanager.go:983-985`):
  - `users==0 && Active()==0`. **جریان‌های خام، از جمله L3، شمرده نمی‌شوند**؛
  - در حالت معکوس، سن لینک ≥ ۴s؛
  - اگر `bornSpare` باشد، سن ≥ ۳۰s؛
  - نگهبان معکوس برقرار باشد: ۴s از کاهش هدف گذشته، دست‌کم یک لینک ≥۴s که pool-control را رد نکرده، و بیرون از churn hold (`engine/linkmanager.go:963-972`).
- **سقف:** `closesPerTick = min(max(2, ceil(R/32)), 8)` در هر تیک (`engine/linkmanager.go:1062-1064`). بستن بیرون از قفل و با لرزش ۵۰ تا ۲۵۰ms انجام می‌شود (`:1031-1034`).
- **بازپس‌گیری** (`reclaimIdle`، `engine/linkmanager.go:1082-1117`):
  - جریان‌هایی که `drainIdle` (پیش‌فرض ۳۱۰s، `:82`) بی‌بایت بوده‌اند، حداکثر ۱۶ تا در هر گذر (`:83`)، با FIN بسته می‌شوند؛
  - پس از `retireForce` = ۲۰ دقیقه، جریان‌های «جاری‌نبودن» هم بسته می‌شوند (`:91`)؛
  - جریانی که بین انتخاب و بستن تکان خورده باشد بخشیده می‌شود.
- **لاگ‌ها:**
  - `%s %d retired: its connections ended (…)` (closeLog، `:1029`)
  - `link %d retiring %s: held by …` (۱۵m سپس هر ۱h، `:993-1004`)
  - `closed %d connection(s) on retiring links …` (دقیقه‌ای، `:1045-1055`)

**`L11` — شروع گرم و فایل warm**
- **نوشتن:** `warmWriter.note` در هر تیک وضعیت صدا زده می‌شود. فقط پس از ۱ دقیقه کارکرد، و فقط با تغییر هدف یا گذشت ≥۱ دقیقه از نوشتن قبلی، می‌نویسد (`cmd/hs2/status.go:236-246`، `300`).
- **خواندن:** فقط هنگام شروع، و فقط اگر فایل ≤۱۵ دقیقه عمر داشته باشد (`cmd/hs2/status.go:201-220`). **فقط اندازهٔ شروع را بالا می‌برد** (`cmd/hs2/main.go:285-290`). نتیجه به `SetWarm` می‌رود، یعنی `ap.T` و `target` (`engine/linkmanager.go:354-362`).
- **لاگ:** `link pool: coming up at %d links, the size it had before this restart …` (`cmd/hs2/main.go:291`).

### ۲.۲ گروه ب: جای‌گذاری و حسگرهای فشار

**`L12` — `Pick` / `pickKey` و کلاهک انفجار**
- **ورودی:** برای هر لینک: `pressed`، `flowing`، `picks` (جای‌گذاری از نمونهٔ قبل) و `users`.
- **لایه‌ها** (`engine/linkmanager.go:1548-1586`):
  - لایهٔ ۰: serving؛
  - لایهٔ ۱: retiring سالم؛
  - لایهٔ ۲: هر لینک زنده، شامل degraded، draining و **suspect**.
  - در همهٔ لایه‌ها، لینکی که `infoDone` نشده رد می‌شود (`:1554`).
- **کلید مرتب‌سازی:**
  - اول لینک بی‌فشار؛
  - سپس کمترین `flowing+picks`؛
  - سپس کمترین `users`؛
  - در تساوی کامل، انتخاب تصادفی (`engine/linkmanager.go:1470-1484`، `1571-1579`).
- **کلاهک انفجار:** لینکی که در ۳ نمونهٔ اخیر ≥ `max(1, perLink/2)` اتصال تازه گرفته، «pressed» حساب می‌شود (`engine/linkmanager.go:1492-1497`).
- **ورودی‌هایی که اصلاً وجود ندارند:** RTT، loss، `ctrlWait` و نرخ.
- **عمل:** `users++` زیر قفل نوشتن `m.mu`. اتصال تا پایانش روی همان لینک سنجاق می‌ماند.

**`L13` — فشار آپلود**
- **ورودی:** Δ`wrBytes` و Δ`wrBlocked`. هر Write بیش از ۱ms در `wrBlocked` شمرده می‌شود (`engine/health.go:183-196`). نسبت `rwnd_limited/busy` از TCP_INFO محلی هم ورودی است.
- **شرط خام:** `perTick(dWr) ≥ 16KiB && blocked/dt ≥ 0.5 && upRwnd < 0.5` (`engine/linkmanager.go:1798`).
- **چسبندگی:** `pressed` وقتی است که ۲ از ۳ نمونه خام مثبت باشند و لینک serving باشد (`engine/linkmanager.go:1799`، `1804-1806`).
- **معنا:** معنای این حسگر به `TCP_NOTSENT_LOWAT=32KiB` وابسته است (`engine/health.go:107-113`). اگر لینک به خاطر پنجرهٔ گیرنده محدود باشد (`rwnd ≥ 0.5`)، **pressed نیست**.

**`L14` — فشار دانلود (رکورد kindStats خروجی)**
- **ورودی:** رکورد ۶۴ بایتی خروجی: tx، txBlocked، busy، rwnd و پرچم‌ها (`engine/stats.go:24-36`).
- **poll:** فقط وقتی لینک در آن تیک ≥16KiB جابه‌جا کرده باشد (`engine/linkmanager.go:1812-1814`، `engine/stats.go:260-268`).
- **`consumeRecord`** (`engine/linkmanager.go:2032-2064`):
  - اولین رکورد فقط پایه است؛
  - شمارندهٔ عقب‌رفته یا فاصلهٔ بیش از ۷s بین رکوردها، پایهٔ تازه می‌سازد؛
  - شرط خام همان شرط آپلود است.
- **چسبندگی:** `dn` وقتی است که ۲ از ۳ نمونه مثبت باشند، رکورد ≤۶s عمر داشته باشد و `statsOK` برقرار باشد (`engine/linkmanager.go:1805`).
- **بازیابی جریان آمار:** اگر جریان آمار روی لینک زنده بسته شود، پس از ۵s دوباره باز می‌شود (`engine/stats.go:100`، `112-120`). خروجی قدیمی هم «unsupported» علامت می‌خورد (`:151-162`).
- **لاگ:** اگر `limited && !raw` باشد، هر ۱۰ دقیقه: `downloads are limited by this server's receive side (a slow reader or small tcp_rmem) … — more links would not help` (`engine/linkmanager.go:2020-2023`).

**`L15` — refill hold**
- **شروع** (`engine/refill.go:94-121`): لینکی برسد در حالی که هیچ لینک زنده‌ای نیست (`aliveLocked()==0`)، و `target ≥ 2`، و بیرون از `quietTill`.
- **عمل:**
  - اتصال‌های TCP تازه در صف می‌مانند تا لینکی با جا پیدا شود؛
  - سقف هر لینک `max(perLink, ceil((users+queue)/T))` است (`engine/refill.go:141-145`، `152-187`)؛
  - **UDP نگه داشته نمی‌شود** (`engine/stream_iran.go:244`).
- **دوره:** `refillTick` = 100ms (`engine/refill.go:56`).
- **پایان** (`engine/refill.go:248-272`):
  - complete: S ≥ T، یا در معکوس سقف خروجی؛
  - limit: ۱۰s؛
  - stalled: ۳s بدون لینک تازه.
- **پس از limit یا stalled:** تا ۱ دقیقه اپیزود تازه‌ای شروع نمی‌شود (`engine/refill.go:321-323`).
- **لاگ‌ها:** `refill: …` (`engine/refill.go:183`، `332-342`).

**`L16` — دروازهٔ info**
- **رفتار:** لینکی که اولین تبادل `kindInfo`اش تمام نشده، در `pickLocked` رد می‌شود (`engine/linkmanager.go:1554`، `engine/stream_iran.go:80`).
- **پایان تبادل:** `openInfo` در پایان اولین تلاش `infoDone=true` می‌گذارد، هر نتیجه‌ای داشته باشد (`engine/peerinfo.go:272`، `291`). مهلت هر تلاش `infoTimeout` = 5s است (`:52`، `302`).
- **تکرار:** تا ۳ تلاش با فاصلهٔ ۱۰s، سپس هر ۱ دقیقه (`engine/peerinfo.go:85-89`، `engine/stream_iran.go:104-122`).
- **نکته:** `OpenRawStream` پیش از اعمال مهلت، SYN را با مهلت ۳۰s خود smux می‌نویسد (smux `session.go:145`، `524-527`). پس روی لینکی که نویسنده‌اش گیر کرده، `infoDone` می‌تواند تا ≈۳۰s عقب بیفتد (نتیجه‌گیری از کد).

### ۲.۳ گروه ج: سلامت لینک

**`L17` — suspect**
- **ورودی:** تغییر `rdBytes` (بایت‌های خوانده‌شده در سطح smux، شامل NOP و pong) (`engine/linkmanager.go:1736-1738`).
- **شرط:** `rxSeen && now − lastRx ≥ 12s` (`engine/linkmanager.go:1740`، `317`). مقدار `lastRx` زمان **تیکی** است که تغییر را دید.
- **عمل:**
  - لینک از لایه‌های ۰ و ۱ pick بیرون می‌رود؛
  - در `countsLocked`، `reconcile` و `apLink.serving` شمرده نمی‌شود؛
  - در `dialRoom` «slot» می‌ماند؛
  - با اولین بایت، در تیک بعد برمی‌گردد.
- **زمان واکنش:**
  - ۱۲ تا ۱۴ ثانیه پس از آخرین بایت دریافتی؛
  - برای لینک بی‌کار که سیاه‌چاله می‌شود، ≈۴ تا ۱۴ ثانیه پس از شروع، چون NOP طرف مقابل هر ۴ تا ۸ ثانیه می‌آید (محاسبه).
- **استثنا:** لینکی که هرگز چیزی دریافت نکرده (`rxSeen=false`) هرگز suspect نمی‌شود.
- **لاگ:** فقط هنگام ورود: `link %d: nothing received for %s — not used for new connections until it answers` (`engine/linkmanager.go:1742`). برای بازگشت لاگی نیست.

**`L18` — کانال کنترل (حسگر stuck و loss دانلود)**
- **آهنگ** (`engine/control.go:102-130`):
  - ping هر ۳s، اگر از تیک قبل ≥96KiB جابه‌جا شده باشد؛
  - ۲ تا ۴ ثانیه بین دو ping، اگر ≥4KiB جابه‌جا شده باشد (تیک ثابت ۳s به‌اضافهٔ تأخیر تصادفی ۰ تا ۱ ثانیه در هر تیک)؛
  - لینک بی‌کار هر ۳ تا ۵ تیک، یعنی ۹ تا ۱۵ ثانیه.
- **ping معلق:** حداکثر ۴ (`:43`).
- **نوشتن:** مهلت ۳s. اگر مهلت تمام شود، ping در smux در صف می‌ماند و کانال ادامه می‌دهد (`:141-148`).
- **خواندن:** مهلت ۶s در هر تیک (`:152`).
- **خروجی‌ها:** `ctrlWait` (زمان قدیمی‌ترین ping بی‌پاسخ)، `ctrlAnsweredSent`، `rttMicros`، `peerRetrans` و `peerLoss` (`:161-177`).
- **پایان:** با خطای غیر timeout پایان می‌یابد و **دوباره باز نمی‌شود**. مقدار `ctrlWait` صفر می‌شود (`:90`، `142-143`، `156-157`).

**`L19` — stuck**
- **ورودی:** `ctrlWait`، جابه‌جایی تیک (`moved`)، `o.wedged` (پارک شدن خواننده در ۶ ثانیهٔ اخیر، `engine/linkmanager.go:1659-1663`)، و «شاهدها».
- **شاهد (`answering`):** لینکی که همهٔ این شرط‌ها را دارد (`engine/linkmanager.go:1895-1900`):
  - `moved ≥ 4KiB`؛
  - نه degraded، نه draining، نه suspect؛
  - `peerSeen`؛
  - `ctrlWait < 2s` و `rtt < 2s`؛
  - `ctrlAns ≠ 0`.
- **شرط «انتظار»:** `!degraded && !draining && !wedged && ctrlWait ≥ 6s && moved < 96KiB` (`engine/linkmanager.go:1877`).
- **رگه:** اگر لینک suspect نباشد، `stuckStreak++`؛ در ≥۲ نامزد می‌شود (`engine/linkmanager.go:1884-1888`).
- **حکم** (`engine/linkmanager.go:1976-1991`): همهٔ این‌ها با هم:
  - دست‌کم یک شاهد وجود داشته باشد؛
  - `!recovering`؛
  - شاهد پاسخ pingی را گرفته باشد که **بعد از** قدیمی‌ترین ping نامزد فرستاده شده (`lastAns > c.sent`)؛
  - `perTick < max(12KiB, میانهٔ moved شاهدها/2)`؛
  - جا باشد: `drainHeadroom(max) − stuckNow`؛
  - ترتیب: طولانی‌ترین انتظار اول.
- **عمل:** `degraded = stuck = true` و `pressed = false`.
- **زمان واکنش (محاسبه، برای لینک پرکار):** ping بعدی ۰ تا ۳ ثانیه پس از شروع، به‌اضافهٔ ۶s، به‌اضافهٔ یک تیک برای رگهٔ ۲، به‌اضافهٔ هم‌ترازی تیک: ≈۸ تا ۱۳ ثانیه.
- **لاگ:** `link %d stuck: its traffic has waited %s for an answer while it moved %s in %s (the other links answer in ~%dms) — draining` (`engine/linkmanager.go:1988`).

**`L20` — مسیر کند یا شلوغ، و پنجرهٔ بهبود**
- **شرط:** `slow = press || waitingN ≥ 2 && (waitingN > len(answering) || inflated)` (`engine/linkmanager.go:1935`). در اینجا:
  - `inflated = usual>0 && len(answering)≥3 && median > max(500ms, 4·usual)` (`:1932`)؛
  - `usual` کمینهٔ کمینه‌های دقیقه‌ای ۱۰ دقیقهٔ اخیر است، و فقط از تیک‌هایی که ≥۳ شاهد داشته‌اند (`engine/stuck.go:97-131`).
- **عمل:**
  - `stuckSlowAt = now` و `stuckSlowFor += dt`. اگر از پنجرهٔ قبلی گذشته باشد، طلسم تازه شروع می‌شود (`:1936-1942`).
  - `recovering` = زمان گذشته از آخرین تیک کند < `stuckRecoverFor = min(max(30s, slowFor), 2m)` (`:1943`، `engine/stuck.go:136-138`).
  - در این حالت **نه حکم loss صادر می‌شود** (`:1948`) **نه حکم stuck** (`:1976`).
  - `calm` (تیک قبلی) رگه‌های loss را صفر می‌کند (`:1703`، `1833`، `1842`).
- **لاگ‌ها** (حداکثر دقیقه‌ای، `:1951-1967`):
  - `… the path or the other server is slow, not those links: none is drained`
  - `… the path is congested, not those links: none is drained`
  - `kernel TCP memory on %s is above its pressure mark …`

**`L21` — loss**
- **آپلود** (`engine/loss.go:98-107`):
  - برای هر تیک، به شرط `tsOK && dWr ≥ 96KiB`؛
  - `frac = Δretrans / ΔData_segs_out`. اگر شمارش هسته نباشد، مخرج `bytes/1400 + Δretrans` است.
- **دانلود** (`engine/loss.go:115-138`):
  - پنجره بین دو pong است؛ پنجرهٔ کوتاه‌تر از ۱s باز می‌ماند؛
  - اگر داده کمتر از `96KiB × win/2s` باشد، پنجره «بسته ولی آرام» است؛
  - `frac = ΔpeerRetrans / (ΔsegsIn + ΔpeerRetrans)`.
- **بد بودن:** `frac > 0.12` (`engine/health.go:31`).
- **رگه:** برای هر جهت جدا. در `!calm` صفر می‌شود. نمونهٔ آرام رگه را نگه می‌دارد، اگر آخرین بد ≤۲۰s پیش بوده باشد (`engine/linkmanager.go:1832-1849`).
- **نامزد:** `streak ≥ 3 && bad` در همین تیک (`:1857`).
- **حکم** (`engine/loss.go:180-225`):
  - اگر ≥۴ لینک داوری شده‌اند و **بیش از نصفشان** پراتلاف‌اند و نرخشان زیر `keep` است ⇒ «مسیر پراتلاف»، و هیچ لینکی تخلیه نمی‌شود (`:192-207`)؛
  - وگرنه، لینکی که ≥ `0.5 × میانهٔ نرخ لینک‌های pressed` جابه‌جا می‌کند (وقتی ≥۴ لینک pressed است) نگه داشته می‌شود؛
  - جا: `drainHeadroom(max) − degradedNow` (`:209`)؛
  - ترتیب: پراتلاف‌ترین اول.
- **عمل:** `degraded = true` و `pressed = false` (`:216`).
- **زمان واکنش (محاسبه):** آپلود: ۳ تیک بد ≈ ۴ تا ۶ ثانیه. دانلود: ۳ پنجرهٔ pong ≈ ۹ ثانیه یا بیشتر.
- **لاگ‌ها:**
  - `link %d degraded (up-loss %v, down-loss %v%s, rtt %dms) — draining` (`engine/loss.go:221`)
  - `%d of %d busy links resend more than %.0f%% — the path is lossy …` (`:202`)

**`L22` — تخلیهٔ degraded**
- **مستقیم (`heal`، `engine/linkmanager.go:2089-2182`)، در همان تیک حکم:**
  - هر `degraded && !draining` به `draining` می‌رود؛ `drainReplace = !retiring` (`:2093-2100`).
  - برای هر لینک serving که degraded شده، اول یک retiring برمی‌گردد. انتخاب: retiring زنده و غیر degraded با بیشترین `open`. **suspect بررسی نمی‌شود** (`:2102-2115`).
  - کسری با dial جایگزین (`replacement=true`) پر می‌شود. سقف: `drainHeadroom(max)` در تیک، و کل لینک‌ها ≤ `max + headroom` (`:2121-2132`).
- **معکوس (`sweepReverse`، `engine/linkmanager.go:1145-1210`):** به‌جای dial، `ctlTarget = T + replacing` به خروجی فرستاده می‌شود (`:1340-1349`). `notifyCtl` **پیش از** بستن‌ها انجام می‌شود (`:1199-1202`).
- **پله‌ها** (`drainStepLocked`، `engine/linkmanager.go:1216-1239`)، در هر تیک:

  | وضعیت | عمل |
  |---|---|
  | `users==0` | بستن فوری |
  | سن > ۹۰s | بستن با همهٔ کاربران باقی‌مانده |
  | سن > ۴۵s، یا stuck | `reclaimStalled` |

  - `reclaimStalled`: جریان‌هایی که ۱۵s بی‌بایت بوده‌اند، با FIN و **موازی** بسته می‌شوند؛ فاصلهٔ شروع‌ها ۲۰ms (`:1361-1402`).
- **پس‌دادن جا** (`slotsBackLocked`، `engine/linkmanager.go:1262-1291`): اگر serving + retiring + در حال dial + جای باقی‌مانده < هدف باشد، قدیمی‌ترین drainingهایی که از ۴۵s گذشته‌اند (یا stuck هستند) بسته می‌شوند.
- **برگشت‌ناپذیری:** هیچ مسیری `degraded` یا `draining` را false نمی‌کند (جستجوی انتساب‌ها در `linkmanager.go` و `loss.go`).
- **لاگ‌ها:**
  - `link(s) %s back in service to replace a degraded link` (`:2128`)
  - `dialed replacement link %d (make-before-break)` (`:942`)
  - `%s %d degraded for %s — …` (`:1223`، `1233`)
  - `%s %d stuck — …` (`:1228`)
  - `%d degraded %s(s) … closed …: the pool is at its ceiling …` (`:1309`)
  - `closed %d connection(s) on degraded links that moved no data for %s …` (`:1396`)
  - `retired %d drained link(s), now %d` (`:2180`)

**`L23` — `reap`، `DropLink` و short-lived**
- **`reap`** (فقط مستقیم): `Alive()==false` ⇒ لینک از پول بیرون می‌رود؛ لاگ `link %d down: %s` (downLog) (`engine/linkmanager.go:1408-1434`).
- **short-lived:** ۳ لینک پیاپی با عمر کمتر از ۲۰s ⇒ یک‌بار راهنمای «tun → icmp» (`:1440-1462`).
- **معکوس:** `DropLink` بی‌درنگ اجرا می‌شود؛ لاگ `reverse link %d from %s down: %s (now %d)` (`:474-496`).
- **جایگزینی:** پرکردن دوباره کار `reconcile` تا T است، نه بر اساس تعداد کاربران.

### ۲.۴ گروه د: dial و هماهنگی دو سر

**`L24` — صف dial و دروازه (مستقیم)**
- **صف** (`engine/linkmanager.go:895-950`):
  - `dialing++` پیش از goroutine انجام می‌شود؛
  - `valid` = epoch عوض نشده و (جایگزین است یا `wantsDial`)؛ `wantsDial` یعنی جا هست و serving < T (`:869-882`).
- **دروازه:**
  - `acquireIf` پیش از رزرو فاصله `valid` را بررسی می‌کند (`engine/dialgate.go:54-75`)؛
  - حداکثر ۸ handshake همزمان؛ فاصلهٔ شروع ۴۰ تا ۱۶۰ms (`:22`، `engine/linkmanager.go:394-396`)؛
  - **جای دروازه تا پایان handshake نگه داشته می‌شود** (`release` پس از `DialLink`، `:915`).
- **`DialLink`:**
  - مهلت connect برابر ۸s (`tlscarrier/carrier.go:162`)؛
  - دست‌دهی و احراز با مهلت ۱۰s (`:193`، `tlscarrier/auth.go:59`)؛
  - `ctx` به dial نمی‌رسد (`engine/mtcp_link.go:286-292`).
- **شکست:** `failStreak++`. اگر ≥۳ باشد، یا هیچ لینک زنده‌ای نباشد، `dialEpoch++` و همهٔ dialهای صف‌شده بی‌تلاش رها می‌شوند (`:920-927`).
- **بدون backoff نمایی:** تیک بعد دوباره صف می‌شود.

**`L25` — slotهای پول خروجی (معکوس)**
- **`setTarget`:** هدف به `[min, max]` خروجی clamp می‌شود؛ `max` هم به سقف اعلام‌شدهٔ لبه پایین می‌آید (`engine/exit_pool.go:144-185`).
  - رشد: slot تازه.
  - کوچک‌سازی: slot وقتی لینکش تمام شد یا پیش از dial، بازنشسته می‌شود (`retireIfOver`، `:190-208`، `331`، `342-352`، `387-397`).
- **backoff dial:** `[d/2, d)`، که d از ۰٫۵s دوبرابر می‌شود تا ۸s (`:48-49`، `59-65`، `364-366`).
- **قطعی:**
  - فقط یک slot «scout» dial می‌کند، با backoff ≤۲s (`:54`، `277-279`) و connect با مهلت ۲s (`cmd/hs2/main.go:534-536`)؛
  - بقیهٔ slotها منتظر `upCh` می‌مانند (`:225-242`).
- **dial دوباره پس از افتادن لینک:** اگر لینک ≥۳۰s زنده بوده، پس از `jitter(0.5s)`؛ وگرنه با backoff دوبرابر (`:404-411`).
- **همهٔ لینک‌ها افتاده:** `want = initial` (`:424-430`).
- **لاگ‌ها:**
  - `no link up to the edge — dials fail …` (`:267`)
  - `a link to the edge is back after %s …` (`:312`)
  - `exit slot %d retired …` / `exit link down (slot %d: %s; now %d); redial` (`:390-401`)

**`L26` — pool-control و نگهبان‌های churn (معکوس)**
- **pool-control** (`engine/exit_pool.go:480-561`):
  - هدف `ctlTarget` فرستاده می‌شود، **هنگام تغییر**: روی دو لینک قدیمی‌تر بی‌درنگ، روی بقیه با تأخیر تصادفی ≤۱٫۵s (`:466`، `543-555`)؛
  - **دوره‌ای**: ۳s±۲۰٪ روی دو لینک سریع، ۳۰s±۲۰٪ روی بقیه (`:32`، `450`، `530-535`، `565-567`)؛
  - مهلت نوشتن ۳s؛
  - EOF روی لینک زنده ⇒ `markPoolRefused` (`:503-508`).
- **سقف پذیرش:** حداکثر `2·max + 8` لینک **زنده**. لینک اضافه ۵s نگه داشته و سپس بسته می‌شود (`engine/stream_reverse.go:36-41`، `68-81`).
- **born spare:** لینکی که وقتی S ≥ T است برسد، retiring به دنیا می‌آید و ۳۰s بسته نمی‌شود (`engine/linkmanager.go:416-418`، `308`، `985`).
- **نگهبان churn:** اگر ۳ ورود مازاد، هر کدام ≤۱۰s پس از یک retire-close، در ۳ دقیقه رخ دهد ⇒ ۱۰ دقیقه هیچ retire-closeی انجام نمی‌شود (`:442-470`، `101-104`).
- **`retireAfterDrop`:** ۴s (`engine/health.go:90`).
- **`growable=false`:** وقتی همهٔ لینک‌های ≥۴ ثانیه‌ای pool-control را رد کرده باشند. در این حالت T به `S+R` محدود می‌شود (`engine/linkmanager.go:1995-2000`، `engine/autopilot.go:834-841`).
- **لاگ:** `the exit redials links this server retires (check the exit's min_links) — keeping %d up for %s` (`engine/linkmanager.go:432`).

### ۲.۵ گروه هـ: لایهٔ نشست، جریان و انتقال

**`L27` — نگهبان wedge**
- **ورودی:**
  - `rdCalls` و `inRead` در `watchConn` (`engine/stream.go:84-115`)؛
  - `wseq` هر رله: فرد بودن یعنی رله در حال نوشتن به برنامهٔ محلی است (`engine/wedge.go:94-113`).
- **پارک بودن:** خواننده در Read نیست، و کمتر از ۲۰۴۸ فراخوانی Read از نگاه قبل داشته (`engine/wedge.go:139-144`، `52`).
- **رلهٔ گیر:** در حال Write است و ≥۶s هیچ Write کاملی نداشته. پیشرفت فقط در نگاه‌ها به‌روز می‌شود، پس عملاً ۶ تا ۸ ثانیه (`:149-155`، `55`).
- **حکم:**
  - `wedged = parked ≥ 3` (یعنی ≥۴s، `:48`، `157`)؛
  - `squeezed = !wedged && memPressure() && stuck > 0` (`:158`)؛
  - در هر دو حالت، رله‌های گیر کشته می‌شوند (`SetLinger(0)` + `Close`، یعنی RST؛ `:159-181`، `298-306`).
  - اگر `wedged` باشد ولی رلهٔ گیری نباشد ⇒ `wedgedEmpty`؛ فقط لاگ.
- **پوشش:** یک goroutine برای همهٔ نشست‌های لبه و خروجی، هر ۲s (`:62`، `210-226`).
- **پیامد جانبی:** `parkedAt` که قاعدهٔ stuck آن را می‌خواند (`L19`).
- **لاگ‌ها** (`engine/wedge.go:266-279`):
  - `mtcp: reset %d connection(s) on %d link(s) whose app had taken nothing for %s while the link's receive buffer was full …` (۳۰s)
  - `… while kernel TCP memory was above its pressure mark …` (۳۰s)
  - `mtcp: %d link(s) stopped reading for several seconds with no stuck connection to release (UDP/TUN backlog or a slow panel dial)` (۱۰m)

**`L28` — فشار حافظهٔ TCP هسته**
- **محلی:**
  - `tcpMemWatch.check` در هر تیک وضعیت (۲s) اجرا می‌شود؛
  - روشن شدن: `mem ≥ tcp_mem[1]`؛ خاموش شدن: `mem < 0.9·tcp_mem[1]`؛
  - سپس `engine.SetTCPMemPressure` (`cmd/hs2/status.go:278-280`، `834-855`).
- **طرف مقابل** (فقط لبه): پرچم `statsFlagMemPressure` در رکوردی که ≤۶s عمر دارد (`engine/linkmanager.go:1669-1676`، `engine/stats.go:238-240`).
- **مصرف‌کننده‌ها:**
  - (۱) `calm=false` و `slow=true`، یعنی همهٔ حکم‌ها تا ۳۰s تا ۲ دقیقه پس از پایان فشار خاموش‌اند (`engine/linkmanager.go:1703`، `1935`)؛
  - (۲) نگهبان در حالت squeezed (`engine/wedge.go:158`).
  - autopilot **مصرف‌کننده نیست** (در `engine/autopilot.go` ارجاعی به آن نیست).
- **لاگ‌ها:**
  - `kernel TCP memory: %d MB in TCP buffers, above the kernel's pressure mark …` / `… back below the pressure mark …` (`cmd/hs2/status.go:849`، `852`)
  - خط دقیقه‌ای در `sampleHealth` (`engine/linkmanager.go:1957`)

**`L29` — `relayDieGrace`**
- **رفتار:** پس از مرگ جریان، رلهٔ کاربر ۵s فرصت دارد بایت‌های باقی‌مانده را تحویل دهد؛ سپس هر دو سر بسته می‌شوند (`engine/wedge.go:68-70`، `328-339`).

**`L30` — keepalive smux**
- **NOP:** هر ۴ تا ۸ ثانیه، تصادفی برای هر نشست (`engine/mtcp_link.go:278`). مهلت نوشتن NOP همان تیک بعدی ping است (smux `session.go:406`).
- **بررسی مرگ:** هر ۲۴s (`engine/mtcp_link.go:279`). اگر از بررسی قبل **هیچ قابی** نرسیده باشد و `bucket > 0` باشد ⇒ `Close` (smux `session.go:409-416`).
  - **اگر bucket ≤ 0 باشد (خواننده پارک شده)، نشست بسته نمی‌شود.**
- **زمان واکنش:** ۲۴ تا ۴۸ ثانیه.

**`L31` — `watchConn` و `TCP_USER_TIMEOUT`**
- **`watchConn`:** اولین خطای Read یا Write ⇒ ثبت دلیل و بستن conn و نشست (`engine/stream.go:90-96`، `179-184`).
- **`TCP_USER_TIMEOUT`:** برابر 20000ms است و روی **هر دو سمت** تنظیم می‌شود (`tlscarrier/tune_linux.go:22`، `46`؛ dial: `tlscarrier/carrier.go:190`؛ پذیرش: `tlscarrier/server.go:74`).
- **keepalive TCP:** ۳s، فقط در سمت dial (`tlscarrier/carrier.go:186-189`). چون NOPهای smux همیشه دادهٔ در راه می‌سازند، این تنظیم حاشیه‌ای است (برداشت من).
- **مهلت نوشتن Carrier:** مهلت ۵ ثانیه‌ای `tlscarrier/carrier.go:27` روی مسیر mtcp **نیست**. smux روی `RawConn()` می‌نویسد که مهلتی ندارد (`tlscarrier/carrier.go:122`).
- **زمان واکنش:** ≈۲۰ ثانیه پس از اولین دادهٔ تأییدنشده، به‌اضافهٔ ≤۲s تا `reap`.

**`L32` — پس‌فشار (کنترل جریان)**
- **پنجرهٔ smux:** هر جریان ۲MiB و هر نشست ۸MiB (`engine/mtcp_link.go:258-264`). recvLoop وقتی `bucket ≤ 0` باشد منتظر می‌ماند (smux `session.go:325-333`).
- **`TCP_NOTSENT_LOWAT`:** ۳۲KiB (`tlscarrier/tune_linux.go:19`، `44`). نویسندهٔ smux زود مسدود می‌شود و همین حسگر `wrBlocked` را معنادار می‌کند.
- **ماهیت:** این‌ها حلقه‌های بستهٔ «سخت‌افزاری» پروتکل‌اند، نه تصمیم hs2. ولی دقیقاً همان چیزی‌اند که wedge (`L27`) و فشار (`L13`/`L14`) از رویشان می‌خوانند.

**`L33` — مهلت‌های SYN/FIN smux و دست‌دهی TLS**
- **smux:** `openCloseTimeout` = 30s برای نوشتن SYN و FIN (smux `session.go:17`، `524-527`، `145`).
  - `openStream` لبه **مهلت جداگانه ندارد** (`engine/stream_iran.go:241-258`).
  - نوشتن سرآیند جریان کاربر (`st.Write(header)`) هم مهلت ندارد.
- **سرور TLS:**
  - دست‌دهی ۱۰s (`tlscarrier/server.go:47`، `86`)؛
  - اولین رکورد ۳۰s (`:40`، `114`)؛
  - احراز ۱۰s (`:131`).

**`L34` — کانال جانبی L3 (hs0)**

| جزء | رفتار | محل |
|---|---|---|
| صف | ۲۵۶ بسته؛ صف پر = دورریز؛ هرگز مسدود نمی‌شود | `engine/l3_link.go:53`، `192-199`، `352-355` |
| کهنگی | بستهٔ ماندگارتر از ۶۰ms دور ریخته می‌شود | `:62`، `207-208` |
| ادغام | نوشتن‌ها تا ۱۶KiB جمع می‌شوند | `:56`، `223-230` |
| مهلت نوشتن | ۵s؛ خطا ⇒ `markDead` | `engine/stream.go:214-218`، `engine/l3_link.go:237-239` |
| مهلت خواندن | ۳۰s | `:78`، `130`، `364-368` |
| keepalive | ۲s، یا ۱۰s±۲۰٪ اگر طرف مقابل `capL3Quiet` بگوید | `:67`، `79`، `131-136` |
| ناظر نشست | هر ۲s؛ اگر `rdCalls` نشست ۱۲s عوض نشود ⇒ `markDead` | `:87`، `143-161` |
| انتخاب لینک | هش rendezvous روی همهٔ `l3Link`های زنده | `:308-323` |
| لاگ دورریز | هر ۳۰s | `:380-393` |

- **باز شدن:** فقط یک‌بار در `OnLink` (`engine/stream_iran.go:126-128`، `419-438`)؛ **دوباره باز نمی‌شود**.
- **وضعیت لینک:** **بی‌خبر از degraded، retiring و suspect.**

**`L35` — جریان‌های UDP کاربر**
- **صف هر جریان:** ۲۵۶ datagram یا ۵۱۲KiB؛ پر بودن یعنی دورریز (`engine/stream_iran.go:286-289`، `406-414`).
- **باز کردن:** جریان بدون hold باز می‌شود (`:338`).
- **خطا:** خطای جریان ⇒ `gone()`؛ datagram بعدی جریان تازه، و احتمالاً لینک تازه، می‌سازد (`:329-383`).
- **بی‌کاری:** جاروب هر ۳۰s؛ جریان بی‌کار بیش از ۲ دقیقه بسته می‌شود (`:307-325`، `engine/stream.go:270`).
- **سمت خروجی:** مهلت خواندن ۲ دقیقه (`engine/stream.go:282`).

**`L36` — مهلت‌های سمت خروجی**
- **نوع جریان:** ۱۰s (`engine/stream.go:51`، `engine/stream_kharej.go:175`، `204`).
- **dial پنل:** ۵s (`engine/stream_kharej.go:233`).
- **نگهبان:** رلهٔ خروجی هم زیر نظر نگهبان wedge است (`:238-244`).

### ۲.۶ گروه و: پردازه و میزبان

**`L37` — `setMemoryLimit`**
- **رفتار:** حد نرم heap برابر نصف RAM، مگر `GOMEMLIMIT` تنظیم شده باشد. یک‌بار در شروع اعمال می‌شود (`cmd/hs2/main.go:305-320`، `327`).
- **ماهیت:** حلقهٔ بازخورد hs2 نیست؛ فقط GC را تهاجمی‌تر می‌کند. **هیچ کنترل پذیرش یا کاهش باری** در hs2 ندیدم: سقفی برای تعداد اتصال کاربر در `serveUserTCP` یا شنونده‌ها نیست (`engine/stream_iran.go:138-161`).

**`L38` — tune و `applyTuning`**
- **ماهیت:** ایستاست و یک‌بار پیش از باز شدن hs0 اعمال می‌شود (جزئیات در `02-cmd-ops-tune.md`).
- **`HS2_TUNE_*`:** فقط آزمایشگاهی است و معنای حسگرها را عوض می‌کند (`cmd/hs2/main.go:256-275`).

**`L39` — `cpuMeter` و `hostMeter`**
- **`cpuMeter`:** فقط لاگ؛ روشن با ≥۹۰٪×هسته‌ها در ۳ نمونه، خاموش با <۷۰٪ (`cmd/hs2/status.go:351-404`).
- **`hostMeter`:**
  - اشباع: ≥۹۰٪ busy یا `PSI10 ≥ 40`، در ۳ نمونه؛
  - رفع: <۷۵٪ و `PSI < 20`، در ۵ نمونه (`cmd/hs2/hostcpu.go:58-65`، `214-242`)؛
  - خروجی فقط به `udpcarrier.SetHostSaturated` می‌رود (`cmd/hs2/status.go:330`).
  - **در l3mtcp هیچ مصرف‌کننده‌ای ندارد.** با grep تنها مصرف‌کننده `udpcarrier/pacer.go:126-130`، `307` بود.

**`L40` — `acceptBackoff`**
- **رفتار:** ۵ms که دوبرابر می‌شود تا ۱s؛ لاگ دقیقه‌ای (`engine/engine.go:211-239`).
- **کاربرد:** برای پورت‌های کاربر (`engine/stream_iran.go:149-157`)، شنوندهٔ لینک خروجی (`engine/stream_kharej.go:116-125`) و شنوندهٔ معکوس (`engine/stream_reverse.go:50-59`).

**`L41` — لاگ‌های تاشونده**
- **`burstLog`:** بیش از ۸ خط در ۱۰s ⇒ خلاصه (`engine/burstlog.go:17-19`).
- **ماهیت:** فقط خوانایی است؛ کنترلی نیست.

---

## ۳. dgtun (جداگانه)

> dgtun هیچ بخشی از مسیر `LinkManager`، smux، نگهبان wedge یا `TCP_USER_TIMEOUT` را ندارد. فقط **همان autopilot**، **همان dialGate** و **همان تعریف flowing** را به اشتراک می‌گذارد. تعریف «pressed» در آن متفاوت است.
>
> `tcpMemPressure` در dgtun هیچ مصرف‌کننده‌ای ندارد: فقط `sampleHealth`، `wedge.go` و `stats.go` در مسیر stream آن را می‌خوانند (grep). در عوض `hostSaturated` فقط در dgtun و udp مصرف می‌شود.

| ID | حلقه | دوره | ورودی ← عمل | واکنش | محل |
|---|---|---|---|---|---|
| `D01` | تیک لبه | ۲s | مستقیم: `sampleHealth → decide → reconcile → scoutIfSilent → publishStats → drainTick`؛ معکوس: `sampleHealth → decide → reconcileReverseEdge → publishStats → drainTick` | — | `engine/dgpool.go:2285-2307`، `1328-1334` |
| `D02` | تیک خروجی معکوس | ۲s | بدون حامل و با هدف بالاتر از warm ⇒ برگشت به warm؛ `reconcile` (فقط dial)؛ scout؛ drain؛ گزارش فشار دانلود | — | `engine/dgpool.go:2310-2337` |
| `D03` | تیک خروجی مستقیم | ۲s | `pruneSticky`، نمونه، `publishDownStats` (بدون autopilot) | — | `engine/dgpool.go:1542-1547`، `2254-2273` |
| `D04` | autopilot مشترک با «pressed» دیتاگرامی | ۲s | `pressed = !gov.Capped && !retiring && warm && droppedAt در همین تیک`. `warm` یعنی `Warm()` یا گذشت ۴s. `droppedAt` از صف پر یا ماندگاری بیش از ۵۰ms می‌آید | مثل `L02`–`L07` | `engine/dgpool.go:1191-1198`، `51-54`، `366-377`، `434-446` |
| `D05` | تا کردن فشار دانلود | ۲s | خروجی تعداد حامل‌های pressed را می‌فرستد؛ لبه به همان اندازه پرکارترین حامل‌ها از نظر دانلود را pressed علامت می‌زند؛ کهنگی ۶s | ۲–۶s | `engine/dgpool.go:1224-1263`، `2029-2052`، `95` |
| `D06` | `reconcile` و dial | ۲s | اول retiring پرکار برمی‌گردد؛ بودجهٔ dial `min(max(4, ceil(T/16)), 20)` و ≤ `max − count − dialing`؛ retire خالی‌ترین‌ها همراه `closeRetire`؛ ۳ شکست پیاپی ⇒ epoch++ | همان تیک | `engine/dgpool.go:1418-1534` |
| `D07` | `drainTick` | ۲s | retiringی که در ۳۰۰ms اخیر جریان نداشته بسته می‌شود؛ spare پس از ۳۰s؛ لبهٔ معکوس پس از ۶s؛ یادآوری `closeRetire` هر ≥۳s | — | `engine/dgpool.go:1551-1604` |
| `D08` | flowlet چسبنده و `retireForce` | هر بسته | مکث ≤۳۰۰ms ⇒ همان حامل؛ پس از ۳۰s retire یا با `avoid` ⇒ هش دوباره با ۴ طبقه | فوری | `engine/dgpool.go:978-1043`، `337-347`، `1608` |
| `D09` | انقضای retire طرف مقابل | ۲s | اعلان تمدیدنشده پس از ۱۲s منقضی می‌شود | ۱۲s | `engine/dgpool.go:274-293`، `1169-1171` |
| `D10` | `muteLoop` | ۲۵۰ms | حاملی ≥۱s ساکت در حالی که دیگری می‌شنود ⇒ mute و دو بار `closeMute`؛ در ۳s ⇒ `closeLink`؛ شنیدن دوباره ⇒ `closeHear`؛ اگر هیچ حاملی نشنود، علامت‌ها برداشته می‌شوند؛ `stallGate`: تیکی که بیش از ۲ دوره دیر برسد، ۵۰۰ms قضاوت را متوقف می‌کند | ≈۱s برای جابه‌جایی جریان، ۳s برای بستن | `engine/dgpool.go:1047-1142`، `74-85` |
| `D11` | اعتبار mute طرف مقابل | — | `avoid` تا ۴s یا تا رسیدن `closeHear` | ≤۴s | `engine/dgpool.go:303-309`، `85` |
| `D12` | scout | ۲s | همهٔ حامل‌ها ≥۳s ساکت و dialی در جریان نیست ⇒ یک dial، هر ≥۵s | ۳–۵s | `engine/dgpool.go:1342-1386` |
| `D13` | کشتن zombie | با هر حامل تازه | حامل‌هایی که ≥۳s ساکت‌اند کشته و از شمارش بیرون می‌روند | فوری | `engine/dgpool.go:674-715` |
| `D14` | `reapLoop` | ۱s | مرده‌ها از `set` حذف می‌شوند | ≤۱s | `engine/dgpool.go:2408-2427` |
| `D15` | مهلت مرگ حامل | — | ۱۵s بدون دریافت ⇒ `errDeadLink` | ۱۵s | `udpcarrier/carrier.go:54`، `318-323` |
| `D16` | انتشار هدف و سقف | ۳s یا تغییر (poll هر ۲۰۰ms) / ۱۵s±۲۰٪ | `TypePoolCtl` | ≤۲۰۰ms | `engine/dgpool.go:2068-2120` |
| `D17` | کنترل نرخ تأخیرمحور | هر بازخورد (≈۱۰۰ms) | هدف صف ۱۰ms، صف پایین ۵ms، base probe ۴s، قاعدهٔ stage | ۱ RTT | `udpcarrier/rate.go:231-232`، `257`، `289-300`، `535`؛ جزئیات در `09-udp-fec.md` |
| `D18` | pacer | پیوسته | سطل ۲ms، بودجهٔ صف ۲۰ms، اعتبار دیرکرد ۱۰ms، اعتبار اشباع ۵۰ms | — | `udpcarrier/pacer.go:82`، `92`، `104`، `120`، `307` |
| `D19` | Governor (policer) | ۵۰۰ms | episodeهای اتلاف همزمان در ۳۰s ⇒ سقف `0.9 × passedRate`؛ بالا بردن هر ۴s؛ استراحت ۵m تا ۱h | ≥۲ episode | `udpcarrier/governor.go:109-130`، `144`، `284`؛ طبق نقشهٔ ۰۹ |
| `D20` | FEC تطبیقی و reorder | هر گزارش / ۱۵ms | جزئیات طبق نقشهٔ ۰۸ و ۰۹ | — | `engine/reorder.go:49-59`، `fec/adapt.go` |
| `D21` | صف عادلانهٔ DRR | هر بسته | quantum ۱۵۰۰ بایت، فراموشی ۲s، جریان تُنُک ≤۳۲KB/s | — | `engine/dgfq.go:49-63` |

**تعامل‌های خاص dgtun** (برای ماتریس بخش ۴):
- **Governor و autopilot:** زیر سقف Governor هیچ حاملی pressed نیست (`engine/dgpool.go:1159`، `1198`)، پس رشد autopilot عمداً خنثی می‌شود.
- **یک دورریز صف، دو برداشت:**
  - autopilot آن را pressed می‌خواند، یعنی «مسیر پر است، حامل بیشتر لازم است» (`:368`، `440`)؛
  - کنترل نرخ همان را `stageDrops` می‌خواند، یعنی «گلوگاه محلی است». به شرط `!limited && fair && !govCapped`، `capEst` را به ۲× آنچه واقعاً بیرون رفته محدود می‌کند (`engine/dgpool.go:381-385`، `udpcarrier/carrier.go:253`، `udpcarrier/rate.go:535`).
  - دو حلقه یک رخداد را به دو معنای متضاد تعبیر می‌کنند. اثر عملی: **نامطمئن**.
- **حامل muted:** تا بسته شدن در ۳s همچنان در `apSample` serving شمرده می‌شود (`engine/dgpool.go:1200`). پس S تا ۳ ثانیه بیش‌برآورد است.
- **forwarderهای dgtun** با `relay` ساده کار می‌کنند و نگهبان wedge ندارند (`engine/dgforward.go:141`). bucket مشترکی بین کاربران هم ندارند، چون هر اتصال درونی TCP خودش روی TUN است.

---

## ۴. ماتریس تعامل (مسیر mtcp / l3mtcp)

**راهنمای «نوع»:**
- **هماهنگ:** طراحی‌شده تا با هم کار کنند.
- **خنثی‌کننده:** یکی جلوی دیگری را می‌گیرد.
- **رقیب:** هر دو روی یک چیز اثر می‌گذارند و ممکن است با هم بجنگند.
- **کور:** یکی از دیگری خبر ندارد.

**قطعیت** فقط دربارهٔ وجود سازوکار در کد است. اثر عملی جداگانه علامت خورده است.

| # | حلقه‌ها | شیء مشترک | نوع | شاهد کد | اثر |
|---|---|---|---|---|---|
| 1 | `L17`/`L19`/`L21` ← `L03` | S (لینک serving) | خنثی‌کننده | suspect، degraded و draining در `apLink.serving` نیستند (`engine/linkmanager.go:2068`، `299-301`)؛ GROW به `S ≥ T` نیاز دارد (`engine/autopilot.go:548`)؛ مسلح شدن به `S ≥ pr.to` نیاز دارد (`:623`) | هر لینک suspect یا degraded تا جایگزینش بالا بیاید رشد را می‌بندد. اگر حین پروب رخ دهد، abort با backoff ۶۰s تا ۸m می‌آید (`:626-643`). اثر عملی: نامطمئن |
| 2 | `L17` ← `L09` (dialRoom) | جای max | خنثی‌کننده | `dialRoomLocked` لینک suspect را slot می‌شمارد (`engine/linkmanager.go:2200-2208`) | در سقف max، لینک suspect جایگزین نمی‌گیرد تا TCP آن را بکشد (≈۲۰–۳۰s) |
| 3 | `L17` ← `L12` (لایهٔ ۲) و `L15` | جای‌گذاری | کور | لایهٔ ۲ suspect را می‌پذیرد (`engine/linkmanager.go:1560-1570`)؛ refill فقط با `alive==0` شروع می‌شود (`engine/refill.go:100`) | وقتی همهٔ لینک‌ها suspect‌اند (قطعی کامل پیش از مرگ TCP)، کاربر تازه به‌جای انتظار روی لینک مرده جا می‌گیرد |
| 4 | `L12` در برابر لینک تازه‌مرده یا گیر | `flowing` در `pickKey` | رقیب | `flowing` با `flowRecent=6s` صفر می‌شود (`engine/mtcp_link.go:117`)؛ کلید کمترین `flowing+picks` را ترجیح می‌دهد (`engine/linkmanager.go:1480-1482`) | از ≈۶s تا رسیدن stuck یا suspect (۸–۱۴s)، لینک خراب «سبک‌ترین» است و کاربر تازه جذب می‌کند. تنها مهار، کلاهک انفجار است (`:1492-1494`) |
| 5 | `L13`/`L14` (استثنای rwnd) ← `L12` و `L03` | `pressed` | کور | فشار با `upRwnd/rwnd < 0.5` حذف می‌شود (`engine/linkmanager.go:1798`، `2057`) | لینکی که گیرنده‌اش کند است (wedge لبه، `tcp_rmem` کوچک) بی‌فشار دیده می‌شود: pick آن را **ترجیح می‌دهد** و autopilot برایش رشد نمی‌کند. فقط راهنمای ۱۰ دقیقه‌ای rwnd لاگ می‌شود |
| 6 | `L27` ← `L19` | `parkedAt` | خنثی‌کننده | `waits` به `!o.wedged` نیاز دارد (`engine/linkmanager.go:1877`)؛ `wedged` یعنی `parkedAt` در ۶s اخیر (`:1659-1663`) | لینک گیر را نگهبان wedge مدیریت می‌کند. اگر نگهبان نتواند آزادش کند (`wedgedEmpty`)، لینک **دائماً** از قاعدهٔ stuck معاف می‌ماند |
| 7 | `L27` ← `L30` | `bucket` نشست | هماهنگ (smux)، ولی بی‌پشتیبان | keepalive با `bucket ≤ 0` نشست را نمی‌بندد (smux `session.go:410-416`) | نشستی که پیوسته wedge است، با keepalive نمی‌میرد |
| 8 | `L27` ← `L34` (ناظر نشست) | `rdCalls` | رقیب یا خنثی‌کننده | ناظر L3 روی `rdCalls` کار می‌کند (`engine/l3_link.go:143-169`)؛ L3 فقط در `OnLink` باز می‌شود (`engine/stream_iran.go:126-128`) | wedge ≥۱۲s، کانال L3 آن لینک را **برای همیشه** حذف می‌کند، حتی اگر نگهبان بعداً لینک را آزاد کند. جریان‌های TUN به لینک‌های دیگر rehash می‌شوند |
| 9 | `L28` ← `L19`/`L21`/`L27` | حکم‌ها و رله‌ها | هماهنگ | `calm`/`slow`/`recovering` (`engine/linkmanager.go:1703`، `1935-1950`)؛ squeezed (`engine/wedge.go:158`)؛ آزمون `TestNoVerdictsUnderMemoryPressure` (`engine/mempressure_test.go:66-98`) | طراحی‌شده: زیر فشار حافظه هیچ لینکی محکوم نمی‌شود و رله‌های گیر آزاد می‌شوند |
| 10 | `L28` در برابر خوانندهٔ کند ولی زنده | حافظهٔ TCP | خنثی‌کننده (شکاف) | نگهبان فقط رلهٔ «گیر» (۶s بدون Write کامل) را می‌کشد (`engine/wedge.go:153`) | اگر حافظه را خوانندگان کند ولی زنده پر کرده باشند، نگهبان آن را آزاد نمی‌کند، فشار می‌ماند و **همهٔ حکم‌های loss و stuck تا پایان فشار و ۳۰s تا ۲m پس از آن خاموش‌اند**. اثر عملی: نامطمئن |
| 11 | `L21` و `L19` | سقف تخلیه | رقیب | loss: `drainHeadroom − degradedNow` (`engine/loss.go:209`)؛ stuck: `drainHeadroom − stuckNow` (`engine/linkmanager.go:1981`)؛ حکم‌های loss همان تیک پیش از stuck اعمال می‌شوند ولی stuck آن‌ها را کم نمی‌کند | در یک تیک، مجموع تخلیهٔ تازه می‌تواند تا ≈۲× headroom برسد. قصد طراحی: نامطمئن |
| 12 | `L21` ← `L13`/`L14` | `pressedRates` | هماهنگ | `keep = 0.5 × میانهٔ dom لینک‌های pressed` (`engine/loss.go:184-191`) | فقط وقتی ≥۴ لینک pressed است. با کمتر از آن، لینک پراتلاف تنها داوری می‌شود |
| 13 | `L20` ← `L21`/`L19` | حکم‌ها | هماهنگ | `recovering` (`engine/linkmanager.go:1943`، `1948`، `1976`) | در طلسم کندی و پنجرهٔ بهبود، هیچ حکمی صادر نمی‌شود (طراحی) |
| 14 | `L22` (`heal`) ← `L05`/`L09` | لینک‌های retiring | هماهنگ، با یک نقص | `heal` retiring را برمی‌گرداند بی‌آنکه suspect را بسنجد (`engine/linkmanager.go:2105`) | لینک retiring که suspect است «جایگزین» می‌شود ولی serving نیست، پس `reconcile` همان تیک باز dial می‌کند (یک un-retire بی‌اثر) |
| 15 | `L22` (dial جایگزین) و `L09` (dial عادی) و `L24` (epoch) | دروازه و epoch | هماهنگ | جایگزین فقط به epoch وابسته است (`engine/linkmanager.go:902-907`)؛ ۳ شکست ⇒ epoch++ (`:921-923`) | سه handshake ناموفق، جایگزین‌های صف‌شده را هم رها می‌کند؛ `reconcile` در تیک بعد کسری را تا T دوباره صف می‌کند |
| 16 | `L10` (بستن retiring) ← `L34` | جریان‌های TUN | کور | جریان L3 خام است (`engine/mtcp_link.go:183`)؛ شرط بستن `Active()==0` است (`engine/linkmanager.go:983`)؛ pick در L3 rendezvous روی همهٔ لینک‌هاست (`engine/l3_link.go:308-323`) | کوچک‌سازی و بستن، جریان‌های TUN را وسط کار جابه‌جا می‌کند. رشد هم با آمدن لینک تازه ≈`1/(n+1)` جریان‌ها را جابه‌جا می‌کند |
| 17 | `L34` در برابر `L17`/`L19`/`L21`/`L10` | وضعیت لینک | کور | `l3Set.pick` فقط `Alive()` کانال L3 را می‌بیند (`engine/l3_link.go:315`) | جریان TUN روی لینک degraded تا بسته شدن لینک (≤۹۰s) می‌ماند. روی لینک suspect یا stuck تا ناظر نشست (۱۲–۱۴s) یا مرگ لینک می‌ماند |
| 18 | `L02` در برابر بار TUN | `flowing` | کور | جریان L3 در `flowStats` نیست (`engine/mtcp_link.go:97-128`، `183`) | FLOOR بار hs0 را نمی‌بیند. بایت‌هایش در `rate`، `G` و `wrBlocked` هست (`engine/health.go:168-196`) و رشد فقط از مسیر پروب ممکن است |
| 19 | `L26` (churn، `retireAfterDrop`، born spare) در برابر `L10` و `L25` | بستن در برابر dial دوباره | هماهنگ | `engine/linkmanager.go:963-972`، `983-985`، `442-470` | جلوگیری از چرخهٔ «لبه می‌بندد، خروجی dial می‌کند» (طراحی) |
| 20 | `L25` (`incLive` ← initial) در برابر هدف لبه | اندازهٔ خروجی | هماهنگ | `engine/exit_pool.go:424-430`؛ refill در ۳s «stalled» می‌شود (`engine/refill.go:262`) | پس از قطعی کامل، خروجی با warm بالا می‌آید و لبه هدف را دوباره می‌فرستد |
| 21 | `L15` در برابر `L02` | سقف refill | هماهنگ | سقف = `ceil(D/T)` (`engine/refill.go:141-145`) | اگر FLOOR در اپیزود T را بالا ببرد، سقف هر لینک پایین می‌آید |
| 22 | `L14` (poll فقط برای لینک پرکار) ← `L28` (فشار طرف مقابل) | رکورد آمار | کور | poll فقط وقتی ≥16KiB در تیک (`engine/linkmanager.go:1812-1814`)؛ کهنگی ۶s (`:1671`) | اگر لینک‌ها بی‌کار باشند، فشار حافظهٔ طرف مقابل دیده نمی‌شود (احتمالاً بی‌اهمیت، چون فشار معمولاً با ترافیک می‌آید؛ نامطمئن) |
| 23 | `L28` و `L39` ← `L03` | رشد پول | کور | autopilot نه `memPressure` می‌خواند نه CPU (grep در `engine/autopilot.go`) | زیر فشار حافظه یا CPU، اگر نویسنده‌ها کند شوند و pressed خوانده شوند، پروب‌ها ادامه می‌یابند. شرط سود افزایشی احتمالاً آن‌ها را رد می‌کند. اثر عملی: نامطمئن |
| 24 | `L32` (`NOTSENT_LOWAT`) ← `L13` | معنای `wrBlocked` | وابستگی ایستا | `engine/health.go:107-113`؛ MPTCP به همین دلیل خاموش است (`/home/user/hs2-/CHANGELOG.md:827-832`) | تغییر `HS2_TUNE_NOTSENT` آستانهٔ فشار را بی‌صدا جابه‌جا می‌کند |
| 25 | `L31`/`L30`/`L17`/`L26` | نردبان زمان | هماهنگ | `suspectAfter=12s` < `USER_TIMEOUT=20s` < keepalive ۲۴s < `bornSpareGrace=30s` (`engine/linkmanager.go:303-317`) | ترتیب عمداً طوری است که جایگزین معکوس پیش از تشخیص مرگ قدیمی بسته نشود |
| 26 | `L22` (`reclaimStalled`) در برابر `L33` (FIN با ۳۰s) | بستن جریان | هماهنگ | بستن موازی (`engine/linkmanager.go:1351-1360`، `1378-1382`) | بستن‌ها پشت سر هم ۳۰s معطل نمی‌شوند |
| 27 | `L18` در برابر `L32` | ping پشت صف لینک | هماهنگ (طراحی حسگر) | ping پشت ترافیک خود لینک است و نوشتن timeoutشده در صف می‌ماند (`engine/control.go:17-28`، `141-148`) | `ctrlWait` همان انتظار کاربران است |
| 28 | `L12`/`L16` در برابر `L33` | `OpenStream` | کور | SYN با ۳۰s؛ سرآیند بدون مهلت (`engine/stream_iran.go:248-252`؛ smux `session.go:145`) | کاربری که روی لینک بی‌علامت ولی گیر جا بگیرد، تا ۳۰s برای هر تلاش، یا تا مرگ لینک، معطل می‌ماند |
| 29 | `L39` ← `L13` | مدت Write | کور | `blockedMin = 1ms` (`engine/health.go:49-53`) | زیر اشباع CPU، Writeهای کند شاید فشار مسیر خوانده شوند. نامطمئن (اندازه‌گیری‌ای ندیدم) |
| 30 | `L24` (dial مستقیم، بدون backoff) در برابر مسیری که TCP را پس از چند KB می‌کشد | dial | خنثی‌نشده | فقط یک راهنمای یک‌باره (`engine/linkmanager.go:1440-1462`) | پول هر تیک dial می‌کند و لینک‌ها کوتاه‌عمرند. حلقه‌ای که dial را کند کند وجود ندارد |

### ۴.۱ ماتریس dgtun (خلاصه)

| # | حلقه‌ها | نوع | شاهد | اثر |
|---|---|---|---|---|
| d1 | `D19` ← `D04` | خنثی‌کننده (طراحی) | `engine/dgpool.go:1159`، `1198` | زیر سقف policer رشد نیست |
| d2 | `D04` در برابر `D17` (stage) | رقیب در تعبیر | `engine/dgpool.go:368`، `440`؛ `udpcarrier/rate.go:535` | یک دورریز هم «حامل بیشتر» معنا می‌دهد هم «سقف نرخ محلی». اثر: نامطمئن |
| d3 | `D10` ← `D04` | کور (کوتاه) | `engine/dgpool.go:1200` | حامل muted تا ۳s serving شمرده می‌شود |
| d4 | `D10` ← `D08` | هماهنگ | `engine/dgpool.go:981`، `1030-1031` | جریان چسبیده در بستهٔ بعدی از حامل muted جدا می‌شود |
| d5 | `D12`/`D13` ← `D10` | هماهنگ | `engine/dgpool.go:1116-1121`، `680-715` | وقتی همه ساکت‌اند، mute قضاوت نمی‌کند و scout کار را به عهده می‌گیرد |
| d6 | `D07`/`D09` ← طرف مقابل | هماهنگ | `engine/dgpool.go:1583-1595`، `284-293` | گم شدن `closeServe`/`closeRetire` با یادآوری و انقضا جبران می‌شود |
| d7 | `L39` ← `D18` | هماهنگ | `cmd/hs2/status.go:330`؛ `udpcarrier/pacer.go:307` | اشباع میزبان، اعتبار ترکیدن pacer را ۵۰ms می‌کند |

---

## ۵. کدام وضعیت خرابی را کدام حلقه می‌گیرد، و کدام را هیچ‌کس

### ۵.۱ l3mtcp (پایه: لبهٔ مستقیم، پول با چند لینک)

| # | وضعیت خرابی | می‌گیرد (اول ← بعد) | زمان | پیامد برای کاربر | چه کسی **نمی‌گیرد** / شکاف |
|---|---|---|---|---|---|
| F1 | سیاه‌چالهٔ کامل یک لینک پرکار | `L19` stuck (اگر شاهد باشد) ← `L17` suspect، `L34` L3 ← `L31` USER_TIMEOUT ← `L30` | ≈۸–۱۳s / ۱۲–۱۴s / ≈۲۰–۲۸s / ۲۴–۴۸s (محاسبه) | اتصال‌های سنجاق‌شده ≈۱۵–۱۷s (مسیر stuck) یا ≈۲۰–۳۳s قطع می‌شوند. کاربر تازه در ۶ تا ۱۴ ثانیهٔ اول جذب می‌شود (ردیف ۴ ماتریس) | بدون شاهد (پول کوچک)، stuck غیرفعال است |
| F2 | سیاه‌چالهٔ لینک بی‌کار | `L17` ← `L31` | ≈۴–۱۴s / ≈۲۰–۲۸s | جذب کاربر تازه تا suspect | — |
| F3 | قطع یک‌طرفهٔ لبه ← خروجی | `L31` روی لبه (دادهٔ تأییدنشده) و `L19`؛ خروجی: `L30`/`L31` | ≈۲۰s | مثل F1 | suspect نمی‌گیرد، چون لبه هنوز NOP خروجی را می‌شنود |
| F4 | قطع یک‌طرفهٔ خروجی ← لبه | `L17` و `L31` | ۱۲–۱۴s / ≈۲۰s | مثل F1 | — |
| F5 | لینکی که پس از احراز هیچ قاب smuxی نمی‌گیرد | `L19` (اگر شاهد باشد) ← `L31` | ≈۸–۱۳s / ≈۲۰s | پس از ≤۵s (`infoDone`) قابل pick است | `L17` هرگز، چون `rxSeen=false` (`engine/linkmanager.go:1736-1740`) |
| F6 | throttle DPI روی چند لینک (چند بسته در ثانیه) | `L19` | ≈۸–۱۵s | تخلیهٔ ۱۵s/۹۰s | اگر اکثریت throttle شوند، `L20` «مسیر کند» اعلام می‌کند و هیچ لینکی تخلیه نمی‌شود |
| F7 | ازدحام یا throttle کل مسیر | `L20` (عمداً هیچ حکمی)؛ `L03` (شکست پروب و backoff) | فوری | — | طراحی: جابه‌جایی کمکی نمی‌کند |
| F8 | لینک پراتلاف پرکار (>۱۲٪ و ≥۹۶KiB در تیک) | `L21` ← `L22` | ≈۴–۶s (آپلود)، ≈۹s+ (دانلود)؛ سپس ۴۵/۹۰s | کاربران فعال تا ۹۰s می‌مانند | مسیر پراتلاف (اکثریت) یا لینکی که با نرخ مسیر کار می‌کند عمداً نگه داشته می‌شود |
| F9 | **لینک پراتلاف کم‌حجم** (<۹۶KiB در تیک، مثلاً تعاملی) | **هیچ‌کس**، مگر اینکه `ctrlWait` به ≥۶s برسد و `L19` بگیرد | — | تأخیر بالا برای کاربران آن لینک | `L21` (دروازهٔ `activeBytes`، `engine/loss.go:258`، `289`)؛ `L12` loss را نمی‌بیند |
| F10 | **لینک با تأخیر ۲ تا ۵ ثانیه** (صف عمیق یک لینک) | **هیچ‌کس** | — | کاربران آن لینک تأخیر چندثانیه‌ای دارند و کاربر تازه هم جذب می‌شود | `L19` به ≥۶s نیاز دارد؛ پاسخ‌های ۲ تا ۶ ثانیه‌ای «به هیچ سو شمرده نمی‌شوند» (`engine/stuck.go:39-43`)؛ `L12` و `L03` RTT نمی‌بینند |
| F11 | یک برنامهٔ کاربر از خواندن بازمی‌ماند | عمداً هیچ‌کس (`TestWedgeGuardSparesLonePausedReader`، `engine/wedge_test.go:195-209`)؛ روی retiring: `L10` پس از ۳۱۰s | — | فقط خود آن کاربر | — |
| F12 | ≥۴ برنامه روی یک لینک کاملاً از خواندن بازمی‌مانند (bucket خالی) | `L27` | ≈۴–۱۲s (آزمون ≤۱۲s: `engine/wedge_test.go:236-239`) | RST برای همان کاربران؛ بقیه ادامه می‌دهند | — |
| F13 | **خوانندگان کند ولی زنده که bucket را خالی نگه می‌دارند** | **هیچ‌کس**: فقط دو لاگ ۱۰ دقیقه‌ای (`wedgedEmpty`، `engine/wedge.go:276-278`؛ راهنمای rwnd، `engine/linkmanager.go:2020-2023`) | — | کل لینک (کاربران دیگر، کنترل، آمار، TUN) به سرعت آن خواننده‌ها محدود می‌شود. لینک «بی‌فشار و سبک» دیده می‌شود و **کاربر تازه جذب می‌کند** | `L27`: Writeها کامل می‌شوند، پس «گیر» نیست (`engine/wedge.go:153`؛ آزمون `TestWedgeGuardTricklingReaderDoesNotHideStall` عمداً خوانندهٔ کند را نگه می‌دارد، `engine/wedge_test.go:240-242`). `L19`: معاف به خاطر `wedged`. `L17`: `rdBytes` آهسته تکان می‌خورد. `L30`: `bucket ≤ 0`. `L34`: `rdCalls` تکان می‌خورد. `L31` در طرف مقابل: پنجره گاه‌به‌گاه باز می‌شود (نامطمئن) |
| F14 | فشار حافظهٔ TCP هسته | `L28` ← `L27` (squeeze) و توقف حکم‌ها | فوری؛ رله‌ها ≈۶–۸s | — | اگر علتش F13 باشد، کسی حافظه را آزاد نمی‌کند و حکم‌ها تا پایان فشار خاموش‌اند (ردیف ۱۰ ماتریس) |
| F15 | wedge سمت خروجی (پنل کند) | `L27` در خروجی، اگر رله کاملاً گیر باشد؛ اگر کند باشد، `L19` لبه کل لینک را تخلیه می‌کند | ≈۴–۸s / ≈۸–۱۵s | در حالت دوم همهٔ کاربران لینک قطع می‌شوند (`engine/stuck.go:55-58`) | هدف‌گیری دقیق ممکن نیست |
| F16 | قطعی کامل مسیر (همهٔ لینک‌ها) | `L17` (همه) ← `L31` ← `L24` (هر تیک) ← `L15` در بازگشت | ۱۲–۱۴s / ≈۲۰–۲۸s | بازگشت سرویس ۱۶–۲۰s پس از پایان قطعی ۴۰ ثانیه‌ای (اندازه‌گیری: `/home/user/hs2-/CHANGELOG.md:813`) | `L19` غیرفعال (شاهدی نیست)؛ کاربر تازه روی لینک suspect جا می‌گیرد (ردیف ۳ ماتریس)؛ dial مستقیم backoff ندارد |
| F17 | ری‌استارت پردازهٔ خروجی (FIN/RST) | `L31` (`watchConn` با EOF) ← `L23` | ≤۲s | قطع و اتصال دوباره | — |
| F18 | ری‌استارت لبه | `L11` (warm) و `L15` | بازگشت ۱۶٫۳s (اندازه‌گیری: `/home/user/hs2-/CHANGELOG.md:812-813`) | — | — |
| F19 | مسیری که TCP را پس از ~۱۰KB می‌کشد | فقط راهنمای `L23` (یک‌بار) | — | لینک‌ها مدام کوتاه‌عمرند | dial کند نمی‌شود؛ تغییر خودکار حامل وجود ندارد |
| F20 | همتا پایین است (شکست dial) | مستقیم: `L24` (epoch، لاگ ۳۰s)؛ معکوس: `L25` (backoff و scout) | هر تیک / ۰٫۵–۸s | — | مستقیم backoff نمایی ندارد |
| F21 | کاربر تازه روی لینکی که نویسنده‌اش گیر است ولی هنوز علامت نخورده | فقط مرگ لینک (`L31`) یا مهلت SYN (`L33`) | تا ۳۰s برای هر تلاش، یا تا مرگ لینک | تأخیر اتصال | `openStream` مهلت ندارد (ردیف ۲۸ ماتریس) |
| F22 | جریان TUN روی لینک degraded، retiring یا suspect | `L34` فقط با مرگ کانال L3 | ۱۲–۱۴s / ۵s / ۳۰s | TUN روی لینک بد تا ۹۰s | `l3Set.pick` کور است (ردیف‌های ۱۶–۱۷ ماتریس) |
| F23 | بار بیش از حد TUN | `L34` (۶۰ms و ۲۵۶ بسته) | فوری | دورریز (طراحی: hs0 برای ترافیک سبک است) | — |
| F24 | **اشباع CPU سرور** | **هیچ‌کس** در l3mtcp (فقط لاگ `L39`) | — | ارسال دیرهنگام | `hostSaturated` فقط به udpcarrier می‌رسد |
| F25 | رشد حافظهٔ پردازه (هزاران اتصال) | فقط `L37` (GC) | — | — | کنترل پذیرش یا سقف اتصال کاربر ندیدم (۱۰٬۰۰۰ اتصال ≈۰٫۹GB RSS، اندازه‌گیری: `/home/user/hs2-/CHANGELOG.md:1091-1093`) |
| F26 | هجوم ناگهانی به پولی که از قبل بالاست | فقط کلاهک انفجار `L12` | — | لینک‌های اول شلوغ می‌مانند (≈۲۳۵ اتصال، `/home/user/hs2-/CHANGELOG.md:1086-1090`) | refill فقط پس از شروع یا قطعی کامل است |
| F27 | پروب بالاتر از `max_links` خروجی (معکوس) | `L07` (abort و backoff) | ۱۵s+ تا ۸m | — | autopilot سقف طرف مقابل را نمی‌داند (`/home/user/hs2-/CHANGELOG.md:1095-1097`) |
| F28 | لینک retiring که با اتصال‌های قطره‌ای نگه داشته شده | `L10` (۳۱۰s بی‌کاری، ۲۰m retireForce) | — | — | اتصال flowing هرگز بسته نمی‌شود (طراحی) |
| F29 | گم شدن پیام pool-control | `L26` (۳s روی دو لینک سریع) | ≤۳٫۶s | — | — |
| F30 | خروجی قدیمی بدون pool-control یا kindStats | `L26` (`growable=false`)، `L14` (unsupported) | — | پول با تعداد ثابت کار می‌کند | — |
| F31 | توقف VM یا پردازه | احتمالاً `L20`: پس از بازگشت همهٔ لینک‌ها منتظرند، پس مسیر «کند» است و حکمی صادر نمی‌شود | — | — | برخلاف dgtun، مسیر stream `stallGate` ندارد. رفتار دقیق: نامطمئن |

### ۵.۲ dgtun (خلاصه)

| وضعیت | می‌گیرد | زمان | شکاف |
|---|---|---|---|
| یک حامل از هر دو سو قطع | `D10` ← `D08` | ≈۱s جابه‌جایی، ۳s بستن | — |
| قطع یک‌طرفه | `D10` + `closeMute` ← `D11` | ≈۱s | — |
| ری‌استارت طرف مقابل بدون bye | `D12`/`D13` | ۳–۵s | — |
| قطعی کامل | `D12` (scout هر ۵s) و `D15` (۱۵s) | — | — |
| policer مسیر | `D19` ← `D04` خاموش | ≥۲ episode در ۳۰s | — |
| کمبود CPU | قاعدهٔ stage در `D17` و اعتبار اشباع در `D18` | — | تعارض تعبیر با `D04` (d2) |
| گم شدن `closeServe` یا `closeHear` | `D09` (۱۲s) / `D11` (۴s) | — | — |
| توقف VM | `stallGate` در `D10` | ۵۰۰ms | — |

---

## ۶. فهرست جمع‌بندی شکاف‌ها (فقط مشاهده، بدون پیشنهاد)

1. **خوانندهٔ کند ولی زنده** (F13): هیچ نگهبانی آن را نمی‌گیرد، لینک کاربر تازه جذب می‌کند و ممکن است فشار حافظهٔ دائمی بسازد (ردیف‌های ۵، ۶، ۷ و ۱۰ ماتریس).
2. **پراتلاف کم‌حجم** (F9) و **تأخیر ۲ تا ۵ ثانیه** (F10): هیچ قاعده‌ای ندارند. `pickKey` و autopilot ورودی RTT یا loss ندارند (`engine/linkmanager.go:1470-1494`؛ `engine/autopilot.go:195-216`).
3. **جذب کاربر تازه به لینک خراب**، پیش از علامت خوردنش (ردیف ۴ ماتریس) و در قطعی کامل (ردیف ۳).
4. **TUN کور به سلامت لینک** (F22) و **حذف دائمی کانال L3** روی لینک زنده (ردیف ۸).
5. **`openStream` بی‌مهلت** (F21).
6. **CPU** (F24) و **حافظهٔ پردازه** (F25): حلقهٔ واکنشی ندارند.
7. **dial مستقیم بی‌backoff** و ادامهٔ dial روی مسیری که TCP را می‌کشد (F19، F20).
8. **سقف تخلیهٔ دوگانه** loss و stuck (ردیف ۱۱).
9. **suspect در سقف max** جایگزین نمی‌گیرد (ردیف ۲)، و `heal` لینک retiring را بدون سنجیدن suspect برمی‌گرداند (ردیف ۱۴).
10. **کانال کنترل** پس از خطای غیر timeout هرگز باز نمی‌شود (`engine/control.go:142-143`، `156-157`). در این حالت لینک تا پایان عمرش نه loss دانلود دارد نه stuck. احتمال وقوع روی لینک زنده: نامطمئن.

---

## ۷. ایده‌های امتحان‌شده و ردشده (مرتبط با همین حلقه‌ها)

| حلقه | نسخهٔ قبلی | مشکل اندازه‌گیری‌شده | جایگزین فعلی | منبع |
|---|---|---|---|---|
| `L02`–`L05` | autopilot نسخهٔ اول با ورودی‌های قفل‌شونده | به ۳۲ رسید و پایین نیامد | همهٔ ورودی‌ها در ۶۰s فراموش می‌شوند | `engine/autopilot.go:41-44` |
| `L08` | میانهٔ **نمونه‌ها** | تخمین ≈۰٫۹Mbit/s شد و ۴۸ لینک «لازم» دیده شد | بهترین مقدار هر **لینک** | `engine/autopilot.go:843-849` |
| `L03` | `spare` ثابت ۴ | رشد در صدها لینک گیر کرد | `min(ceil(p/4), max(4, ceil(p/16)))` | `engine/autopilot.go:279-283` |
| `L21` | loss دانلود از pong آخر روی تیک ۲s | حکم تابع لرزش pong بود | پنجرهٔ بین دو pong | `engine/loss.go:17-25`؛ `/home/user/hs2-/CHANGELOG.md:992-1000` |
| `L21` | مخرج بایت/۱۴۰۰ | ۱٫۱ تا ۱۰× بیش‌برآورد | شمارش segment از TCP_INFO | `engine/loss.go:26-29`؛ CHANGELOG `:1001-1004` |
| `L21` | داوری تک‌لینکی بی‌سقف | مسیر پراتلاف همهٔ ۳۰۰ لینک را تخلیه کرد | آزمون مسیر، نرخ مسیر و headroom | `engine/loss.go:31-46`؛ CHANGELOG `:1005-1007` |
| `L21` | مقایسه با میانهٔ لینک‌های پرکار | پراتلاف‌ها میان throttleشده‌ها پنهان ماندند | مقایسه با نرخ لینک‌های pressed | `engine/loss.go:48-51` |
| `L21`/`L20` | قضاوت loss بلافاصله پس از فشردگی | ۴ تا ۸ لینک تخلیه شد | پنجرهٔ بهبود | `engine/linkmanager.go:1819-1823`؛ CHANGELOG `:945-949` |
| `L18` | پایان کانال کنترل با یک timeout نوشتن | قاعدهٔ loss کور شد | ادامه با ≤۴ ping معلق | `engine/control.go:145-147`؛ CHANGELOG `:950-952` |
| `L19` | برش‌های اولیهٔ قاعدهٔ stuck | ۲۱۰، ۵۵ و سپس ۲۷ لینک در فشردگی تخلیه شد | شاهد و پنجرهٔ بهبود | CHANGELOG `:964-966` |
| `L20` | آزمون مسیر کند فقط شمارشی | ۳۳ لینک در ازدحام تخلیه شد | + تورم RTT | `engine/linkmanager.go:1916-1922`؛ CHANGELOG `:1020-1027` |
| `L20` | شمردن کندپاسخ‌ها، پرکارها و بی‌پاسخ‌ها به‌عنوان «کند» | چند لینک پراتلاف هر دو قاعده را برای همیشه خاموش کردند | فقط منتظرهای سبک | `engine/stuck.go:39-43` |
| `L22` | بستن همهٔ کاربران پس از ۴۵s | ۶۰ تا ۹۰ اتصال فعال در هر لینک قطع شد | پله‌های ۴۵/۱۵/۹۰ | `engine/health.go:72-73`؛ CHANGELOG `:833-835` |
| `L22` | نگه داشتن کاربران تا ۵ دقیقه | p90 بین ۱٫۶ و ۲٫۷s | سقف ۹۰s | `engine/health.go:67-71`؛ CHANGELOG `:842-843` |
| `L27` | نگهبان بدون آستانهٔ `starveCalls` | خوانندهٔ آهسته توقف را ۲۳s+ پنهان کرد | <۲۰۴۸ Read = پارک | `engine/wedge_test.go:211-214` |
| `L28` | نگهبان فقط با bucket پر | ۱۱ لینک سالم تخلیه شد و ۴۴ کاربر قطع شدند | حالت squeezed و توقف حکم‌ها | `engine/mempressure.go:5-13` |
| `L32` | MPTCP پیش‌فرض Go 1.24+ | `notsent_lowat` نادیده گرفته شد؛ p99 به ۸s رسید | MPTCP خاموش | CHANGELOG `:827-832` |
| `L31` | تشخیص مرگ فقط با keepalive smux | معطلی ۱۵s یا بیشتر | `watchConn` | `engine/stream.go:74-76` |
| `L34` | رهاسازی کانال L3 پس از ۲۴ تا ۳۰s | — | سکوت ۱۲ ثانیه‌ای نشست | `engine/l3_link.go:82-86` |
| `L15` | سقف refill که با زمان بالا برود | ۱۹۲ اتصال روی پرترین لینک | سهم منصفانهٔ ثابت (۹۶) | `engine/refill.go:30-34` |
| `L24` | رها کردن صف با یک شکست | ramp با شکست ۱ تا ۵٪ گیر کرد | ۳ شکست پیاپی | `engine/linkmanager.go:196-200` |
| `L26` | pool-control هر ۳s روی همهٔ لینک‌ها | صدها پیام در ثانیه | ۲ لینک سریع، ۳۰s برای بقیه، پخش ۱٫۵s | `engine/exit_pool.go:445-466` |
| `L12` | فشار بدون جبران تأخیر | انفجار اتصال روی چند لینک «آزاد» | کلاهک انفجار | `engine/linkmanager.go:1486-1491` |
| `D10` | حامل mute تا ۱۵s نگه داشته می‌شد | ۱۵s قطعی برای هر کاربر آن حامل | mute در ۱s و بستن در ۳s | `engine/dgpool.go:71-73` |
| `D09` | `closeServe` گم‌شده برای همیشه | حامل همیشه «retiring» می‌ماند | انقضای ۱۲s | `engine/dgpool.go:274-279` |

**بررسی‌شده و عمداً تغییرنیافته** (CHANGELOG `:1085-1097`):
- هجوم به پولی که از قبل بالاست؛
- انفجار بیش از ۴۰ اتصال در دقیقه برای هر لینک، که فشار را از جای‌گذاری پنهان می‌کند (۶ تا ۱۰٪ اتصال‌ها روی لینک throttle می‌افتند)؛
- پروب در حالت معکوس که سقف خروجی را نمی‌داند.

---

## ۸. آزمون‌ها: چه چیزی تضمین شده و چه چیزی نه

**تضمین‌شده** (نام‌ها از `grep '^func Test'`؛ بخشی از آن‌ها اجرا و سبز شدند):
- **wedge:**
  - آزادسازی ۵ رلهٔ گیر بدون آسیب به رلهٔ سالم (`engine/wedge_test.go:155`)؛
  - رلهٔ تنهای مکث‌کرده آزاد نمی‌شود (`:198`)؛
  - خوانندهٔ قطره‌ای توقف را پنهان نمی‌کند و خودش هم بسته نمی‌شود (`:217`، غیرکوتاه)؛
  - پایان رله با مرگ جریان (`:254`).
- **فشار حافظه:**
  - squeeze محلی و طرف مقابل (`engine/mempressure_test.go:24`، `50`)؛
  - نبود حکم زیر فشار و تا `stuckRecover` پس از آن (`:66`)؛
  - کهنه شدن فشار طرف مقابل (`:101`).
- **stuck:**
  - ۲۰ آزمون، از جمله زیرآزمون‌های `wedged by its own users`، `suspect: nothing received`، `every link waits`، `moves its share`، `squeeze …` و `outage: an answer that arrived later…` (`engine/stuck_test.go:216-520`)؛
  - پنجرهٔ بهبود (`:744`، `869`، `931`، `975`)؛
  - loss در طلسم کند (`:1019`)؛
  - کف throttle شبانه (`:1214`)؛
  - مسیر شلوغ (`:1296`).
- **L3:**
  - جابه‌جایی فقط جریان‌های لینک مرده (`engine/l3_link_test.go:43`)؛
  - pump هرگز مسدود نمی‌شود (`:115`)؛
  - دورریز بستهٔ کهنه (`:169`)؛
  - keepalive آرام فقط با cap طرف مقابل (`:195`)؛
  - رهاسازی با سکوت نشست (`:224`).
- **کنترل:**
  - آهنگ ping (`engine/control_cadence_test.go:71`)؛
  - pong دیررس کانال را نمی‌کشد (`:92`).
- **بقیه:** autopilot، loss، refill، dialgate و `pool_v2` (فهرست کامل در `04-linkmanager.md` §۱۰ و `05-autopilot-health.md` §۱۰).

**بدون آزمون مستقیم** (تا جایی که دیدم):
- خوانندگان کند ولی زنده که **هم‌زمان** bucket را خالی نگه می‌دارند؛
- اثر suspect روی `dialRoom` و GROW و abort پروب؛
- لایهٔ ۲ pick با لینک suspect در قطعی کامل؛
- تعامل TUN با degraded/retiring؛
- نبود باز شدن دوبارهٔ L3 یا کانال کنترل روی لینک زنده؛
- مجموع تخلیهٔ loss و stuck در یک تیک؛
- `openStream` روی نویسندهٔ گیر؛
- توقف VM در مسیر stream؛
- تعارض stage و autopilot در dgtun.

---

## ۹. موارد نامطمئن

- **رفتار `TCP_USER_TIMEOUT` در طرف مقابل وقتی پنجرهٔ گیرنده صفر یا قطره‌ای است** (F13). توضیح کد (`engine/wedge.go:20-24`) می‌گوید طرف مقابل پس از ۲۰s پنجرهٔ صفر لینک را رها می‌کند. اینکه با پنجره‌ای که گاه‌به‌گاه باز می‌شود هم چنین شود، به نسخهٔ هستهٔ لینوکس بستگی دارد و بررسی نکرده‌ام.
- **اثر عملی ردیف‌های ۱، ۱۰، ۱۱، ۲۲، ۲۳ و ۲۹ ماتریس، و d2 در dgtun.** سازوکار در کد قطعی است، ولی اندازه‌گیری ندیدم.
- **زمان‌های «محاسبه»** (FLOOR ≈۸–۱۶s، stuck ≈۸–۱۳s، loss، SHRINK ≈۱۲۰s) از خواندن کد درآمده‌اند و ممکن است با هم‌ترازی واقعی تیک‌ها کمی فرق کنند.
- **رفتار مسیر stream پس از توقف VM** (F31).
- **ثابت‌های Governor و FEC در dgtun** فقط تا حد مقدار و path:line در `udpcarrier/governor.go:109-130` بازبینی شد. منطق کامل آن‌ها طبق نقشهٔ ۰۹ است.
