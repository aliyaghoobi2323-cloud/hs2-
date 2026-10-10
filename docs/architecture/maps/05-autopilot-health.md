# autopilot و سلامت و نگهبان‌ها

> دامنه: `engine/autopilot.go` (۹۳۸ خط)، `engine/health.go`، `engine/health_linux.go`، `engine/health_other.go`، `engine/stuck.go`،
> `engine/dialgate.go`، `engine/refill.go`، `engine/burstlog.go`، `engine/loss.go`، `engine/wedge.go`، `engine/mempressure.go` (همه کامل خوانده شد)
> و آزمون‌هایشان: `autopilot_test.go`، `autopilot_sim_test.go`، `autopilot_scale_test.go`، `health_test.go`، `refill_test.go`، `dialgate_test.go`،
> `loss_test.go`، `wedge_test.go`، `mempressure_test.go` (کامل). برای فهم ورودی‌ها این‌ها هم خوانده شد: `engine/linkmanager.go` (بخش‌های
> `sampleHealth`، `consumeRecord`، `reconcile`، `heal`، `drainTick`، `pickLocked`، `publishStats`)، `engine/stats.go` (کامل)، `engine/control.go`،
> `engine/mtcp_link.go` (`flowStats`، تنظیم smux)، `engine/stream.go` (`newSession`، `watchConn`)، `engine/l3_link.go` (`l3Set.pick`)،
> `engine/exit_pool.go` (سرآغاز و `setTarget`/`openPoolCtl`)، `cmd/hs2/status.go` (`tcpMemWatch`)، `cmd/hs2/main.go` (`linkEnvelope`، `warmLinks`)،
> `README.md`، `CHANGELOG.md` (فازهای H و Q)، `hs2-src/BUILD.md`، `hs2-src/VALIDATION.md`.
>
> همهٔ مسیرها نسبت به `/home/user/hs2-/hs2-src` است مگر خلافش گفته شود. ثبت پایه: `0812bc9`. هیچ فایلی در مخزن تغییر نکرد
> (`git status` تمیز). آزمون‌های مرتبط در حالت کوتاه اجرا شد و گذشت:
> `go test -short -run 'Autopilot|TestSim|Probe|Backoff|CapEstimate|ClosesPerTick|Refill|DialGate|Loss|WedgeGuard|RelayEnds|Memory|Degrade|NoDegrade|PickSkips' ./engine/` ⇒ `ok … 9.602s`.
>
> نقشهٔ کلی `LinkManager` در `04-linkmanager.md` است؛ این فایل روی **مغز اندازه‌گیری و تصمیم** (autopilot) و **سیگنال‌های سلامت** متمرکز است و
> هرجا لازم بود به آن ارجاع می‌دهد.

---

## ۰. خلاصهٔ فشرده

- `autopilot` یک **هستهٔ تصمیم خالص** است: نه قفل، نه سوکت، نه ساعت؛ هر ۲ ثانیه یک `apSample` می‌گیرد و یک عدد بیرون می‌دهد: **T = تعداد لینک‌های serving** (لینک‌هایی که اتصال تازه می‌پذیرند) (`engine/autopilot.go:11-21`). `LinkManager` محرک (actuator) است: un-retire، dial، retire.
- همین autopilot **هم** پول stream (mtcp / l3mtcp / tls) **و هم** پول datagram (`dgPool`، dgtun) را اندازه می‌گیرد (`engine/linkmanager.go:338`، `engine/dgpool.go:600`, `engine/dgpool.go:1390`). هر تغییری در آن هر دو را عوض می‌کند.
- autopilot فقط روی **لبه (ایران)** اجرا می‌شود؛ در حالت معکوس، لبه T را از کانال `kindPool` به خروجی می‌فرستد و خروجی فقط پیروی می‌کند (`engine/exit_pool.go:14-30`، `engine/exit_pool.go:161-185`).
- T از هفت مرحله به ترتیب اولویت می‌گذرد (`engine/autopilot.go:340-615`):
  ۰. **CONFIRM** (دورهٔ آزمایشی سودِ پروب پس از «مسیر پر») → ۱. **FLOOR** (`ceil(flowing/per_link)`، بی‌درنگ، بدون پروب) → ۲. **پروبِ در جریان** (`judge`) → ۳. **RESTORE** (برگرداندن کوچک‌سازی‌ای که فوراً کمبود ساخت + hold) → ۴. **GROW** (پروب ~۲۵٪، یا ~۵۰٪ در زنجیره) → ۵. **SHRINK** (پس از ۶۰ ثانیه زیر هدف، هر ۳۰ ثانیه نصفِ فاصله) → ۶. hold/steady.
- **رشد فقط با پروب** و فقط وقتی لینک‌ها «pressed» (فرستنده‌شان منتظر شبکه) باشند و لینک بی‌فشاری برای جریان‌های تازه نمانده باشد (`spare(P)`). پروب فقط اگر ترافیک لینک‌های تازه **به کل افزوده شود** نگه داشته می‌شود؛ وگرنه «مسیر پر» ⇒ عقب‌نشینی نمایی ۳۰ ثانیه → ۸ دقیقه با ±۲۰٪ لرزش.
- **هیچ ورودی‌ای قفل نمی‌شود** (همه در ۶۰ ثانیه فراموش می‌شوند)؛ فقط تخمین ظرفیت هر لینک پنجرهٔ ۳۰ دقیقه‌ای دارد و فقط «چند لینک برای این throughput لازم است» را مقیاس می‌کند (`engine/autopilot.go:41-44`).
- سیگنال‌های ورودی از `sampleHealth` می‌آیند (`engine/linkmanager.go:1619-2024`): نرخ هر لینک (بایت‌شمار `meteredConn`)، `flowing` (EWMA نرخ هر جریان کاربر)، `pressed` (مسدود بودن نویسنده ≥۵۰٪ زمان + ≥۱۶KiB در تیک + سهم rwnd زیر ۵۰٪؛ ۲ از ۳ نمونه؛ آپلود از سوکت خودی، دانلود از رکورد `kindStats` خروجی).
- **autopilot از RTT، loss، notsent، delivery_rate استفاده نمی‌کند.** loss/RTT فقط برای قواعد سلامت (degrade/stuck) است.
- نگهبان‌های سلامت: **loss** (≥۱۲٪ بازارسال، ۳ نمونه، با استثنای «لینکی که نرخ مسیر را می‌برد» و «مسیر پراتلاف»)، **stuck** (پینگ کنترل ≥۶ ثانیه منتظر، ۲ نمونه، با شاهد سالم)، **slow-path** (توقف قضاوت + پنجرهٔ بازیابی ۳۰ ثانیه تا ۲ دقیقه)، **wedge guard** (خوانندهٔ smux پارک‌شده ≥۳ نگاه ⇒ ریست رله‌هایی که ≥۶ ثانیه چیزی نگرفته‌اند)، **فشار حافظهٔ TCP** (توقف همهٔ قضاوت‌ها + ریست رله‌های گیر بدون انتظار پر شدن بافر)، **refill hold** (پخش اتصال‌های تازه پس از شروع/قطع کامل)، **dial gate** (≤۸ handshake همزمان، فاصلهٔ ۴۰ تا ۱۶۰ میلی‌ثانیه، ~۱۰ در ثانیه).
- در `l3mtcp`، ترافیک TUN روی جریان خام (raw stream) هر لینک سوار است: **در `flowing` و در کف (floor) شمرده نمی‌شود**، ولی در نرخ لینک، G و فشار (`wrBlocked`) شمرده می‌شود (جزئیات در بخش ۱۳).

---

## ۱. نقش و جایگاه در کل سیستم

```
             ┌────────────────────── Iran edge (LinkManager.Run / runAccept, every 2 s) ─────────────────────┐
user conns → │ Pick/pickHeld (pickKey: unpressed first)          sampleHealth ──► apSample ──► autopilot.decide │
             │      │                                             ▲  ▲  ▲                        │            │
             │      ▼                                             │  │  │                        ▼            │
             │  link i: smux session over meteredConn ─ bytes ────┘  │  │                 setTarget(T)        │
             │     ├─ countedStream (user)  → flowStats ─────────────┘  │                        │            │
             │     ├─ kindCtrl ping/pong (RTT, exit retrans, ctrlWait)──┘ (loss/stuck)   direct: reconcile → queueDial → dialGate
             │     ├─ kindStats poll → exit record (tx, txBlocked, rwnd) ──► download pressure                 │
             │     ├─ kindPool (reverse) ──► exit: exitPool.setTarget (clamped to its [min,max])                │
             │     └─ kindL3 raw stream (l3mtcp: TUN side channel, rendezvous hash)                            │
             └──────────────────────────────────────────────────────────────────────────────────────────────┘
      wedge guard: one process-wide goroutine, every 2 s, over every smux session (edge and exit)
      tcpMemWatch (daemon status writer, every 2 s) ──► SetTCPMemPressure ──► guard + health rules
```

- **چه کسی autopilot را صدا می‌زند:** فقط `LinkManager.decideTarget` (`engine/linkmanager.go:590-607`) در حلقهٔ مستقیم (`Run`, `engine/linkmanager.go:574-581`) یا معکوس (`runAccept`, `engine/linkmanager.go:1132-1138`)، و `dgPool` (`engine/dgpool.go:1390`). هر دو تیک ۲ ثانیه‌ای (`healthTick`, `engine/health.go:18`).
- ترتیب تیک مستقیم: `reap → sampleHealth → heal → autoscale(decideTarget+reconcile) → drainTick → publishStats` (`engine/linkmanager.go:575-580`).
  ترتیب تیک معکوس: `sampleHealth → reconcile(decideTarget) → drainTick → sweepReverse → publishStats` (`engine/linkmanager.go:1133-1137`).
- `pin` (فقط آزمون‌ها) autopilot را دور می‌زند (`engine/linkmanager.go:591-595`).
- خروجی autopilot: `apDecision{target, phase, reason, note}` (`engine/autopilot.go:219-224`). `note` یک خط لاگ `mtcp: pattern …` است؛ `reason` و `phase` در مانیتور زنده (`PoolStats.Reason/Phase`, `engine/linkmanager.go:2297-2309`).

---

## ۲. اجزای اصلی

### ۲.۱ autopilot (`engine/autopilot.go`)

| نوع/تابع | path:line | نقش |
|---|---|---|
| `autopilot` | `engine/autopilot.go:45-83` | وضعیت: `min,max,perLink`؛ `T` (هدف serving)؛ `hist` (حلقهٔ ۳۰ تیک)؛ `caps` (بهترین نرخ هر لینک وقتی pressed)؛ `pr` (پروب در جریان)؛ `k` (توان عقب‌نشینی)؛ `chain` (موفقیت‌های پیاپی)؛ `aborts`؛ `confirm`؛ `ceil`؛ `next` (پروب بعدی نه زودتر از)؛ `fail{at,g,flows}`؛ `belowSince`، `lastGrowAt`، `lastShrinkAt`، `shrinkFrom`؛ `hold{n,g,until}`؛ `undos`؛ `phase,reason,cCap,gPeak` |
| `apTunables` / `defaultTunables` | `engine/autopilot.go:86-124` / `126-167` | همهٔ ساعت‌ها و آستانه‌ها (جدول بخش ۴). هیچ‌کدام از پیکربندی/محیط خوانده نمی‌شود |
| `apPhase` (`steady/scaling/probing/holding/shrinking`) | `engine/autopilot.go:169-192` | فقط برای مانیتور |
| `apLink` | `engine/autopilot.go:195-206` | یک لینک در یک تیک: `id, serving, retiring, servingSince, pressed, rate, rate10, sustained, flowing, open` |
| `apSample` | `engine/autopilot.go:209-216` | `now, links, G (bytes/s کل), flowing, open, growable` |
| `apDecision` | `engine/autopilot.go:219-224` | خروجی |
| `apTick` | `engine/autopilot.go:226-231` | یک خانهٔ تاریخچه: `g, flowing, p (pressed serving), s (serving), short` |
| `apCap` | `engine/autopilot.go:233-236` | `t` (آخرین نمونهٔ pressed)، `v` (بهترین `sustained`) |
| `apProbe` | `engine/autopilot.go:238-257` | `from,to,start,armed,armedAt,settled,gb,varB,nb,before,evalG,evalNew,evalProbe,evalShort,triggerP,triggerS` |
| `newAutopilot` | `engine/autopilot.go:259-277` | `min≥1`، `max≥min`، `perLink` پیش‌فرض ۸؛ `capMax = max(256, 4*max)`؛ `T = warmSize(min,max)` |
| `spare(p)` | `engine/autopilot.go:284-296` | چند لینک بی‌فشار باید برای p لینکِ pressed بماند |
| `armTimeoutFor` | `engine/autopilot.go:310-312` | `15s + (to−from)×150ms` |
| `decide` | `engine/autopilot.go:340-615` | تصمیم هر تیک (مراحل ۰ تا ۶) |
| `judge` | `engine/autopilot.go:618-780` | اجرای پروب: مسلح‌شدن، نشست، اندازه‌گیری، حکم |
| `backoff` | `engine/autopilot.go:784-791` | `30s << (k−1)` سقف ۸ دقیقه، ضرب در `[0.8, 1.2]` |
| `startConfirm` | `engine/autopilot.go:800-811` | دورهٔ آزمایشی سود پس از «مسیر پر» |
| `apCeil` / `apConfirm` | `engine/autopilot.go:814-818` / `821-830` | سقف مسیر (n لینک، g بایت/ث) / وضعیت آزمایشی |
| `out` | `engine/autopilot.go:834-841` | clamp به `[min,max]`؛ در `!growable` سقف `S+R` |
| `noteCap` | `engine/autopilot.go:850-870` | ثبت بهترین نرخ پایدار هر لینک pressed |
| `capEstimate` | `engine/autopilot.go:876-890` | میانهٔ آن‌ها در پنجرهٔ ۳۰ دقیقه، اگر ≥۶ لینک |
| `mean/meanVarF/meanVar` | `engine/autopilot.go:892-921` | آمار |
| `fmtDur` / `mbitps` | `engine/autopilot.go:924-935` / `938` | قالب‌بندی |

### ۲.۲ اندازه‌گیری و سلامت

| نوع/تابع | path:line | نقش |
|---|---|---|
| `linkMeter` | `engine/health.go:103-157` | شمارنده‌های اتمی هر لینک: `rdBytes, wrBytes, stalls, wrBlocked`؛ آمار خروجی `statsPoll/statsState/statsSeq/peer`؛ `peerMax`، `peerInfo`، `infoDone`، `infoRefused`؛ `guard`؛ `peerRetrans`، `peerLoss`، `rttMicros`، `peerSeen`، `ctrlWait`، `ctrlAnsweredSent` |
| `peerLossRec` | `engine/health.go:160-166` | خوانش هر pong: `n, rt, segsIn, rd, at` |
| `meteredConn` | `engine/health.go:170-196` | Read/Write را می‌شمارد؛ مدت هر Write بالای `blockedMin` (۱ms) به `wrBlocked` افزوده؛ خطای Write ⇒ `stalls++`. **بالای shaper و زیر smux** است (`engine/stream.go:174-177`) پس payload واقعی (شامل قاب‌های smux همهٔ جریان‌ها: کاربر، کنترل، آمار، L3) را می‌شمارد، نه padding |
| `tcpStat` | `engine/health.go:199-212` | یک عکس TCP_INFO: `retrans, busyUs, rwndUs, sndbufUs, deliveryRate, notsent, chronoValid, segsOut, segsIn` |
| `metered` | `engine/health.go:217-220` | لینکی که `meter()` و `tcpStats()` دارد؛ لینک بدون آن هرگز degrade/pressed نمی‌شود |
| `tcpStats` (لینوکس) | `engine/health_linux.go:17-46` | `GetsockoptTCPInfo`؛ `chronoValid = Busy_time > 0` |
| `tcpStats` (غیرلینوکس) | `engine/health_other.go:10-12` | همیشه `false` ⇒ loss و فشار آپلود خاموش |
| `flowSnap` / `mtcpLink.flowStats` | `engine/mtcp_link.go:76-81` / `97-130` | `open, flowing, recent, last` برای هر لینک |
| `managedLink` (فیلدهای نمونه‌گیر) | `engine/linkmanager.go:239-296` | `prev*`، `goodput`، streakها، `upHist/dnHist` (۳ بیت)، `pressed`، `rates[5]`، `doms[3]`، `rate/rate10/sustained`، `flowing/open/recent`، `picks/pickHist` |
| `sampleHealth` | `engine/linkmanager.go:1619-2024` | ساخت `apSample` و همهٔ حکم‌های loss/stuck |
| `consumeRecord` | `engine/linkmanager.go:2032-2064` | رکورد خروجی ⇒ نمونهٔ «دانلود pressed» |
| `apLink()` | `engine/linkmanager.go:2066-2073` | تبدیل `managedLink` به `apLink` |

### ۲.۳ نگهبان‌ها

| نوع/تابع | path:line | نقش |
|---|---|---|
| ثابت‌ها و `rttFloor` | `engine/stuck.go:61-131` | قاعدهٔ stuck و «زمان معمول» RTT |
| `stuckRecoverFor` | `engine/stuck.go:136-138` | `min(max(30s, stuckSlowFor), 2m)` |
| `lossDir`، `judgeUpLoss`، `judgeDownLoss`، `lossVerdicts` | `engine/loss.go:71-225` | قاعدهٔ loss |
| `sessGuard`، `relayWatch`، `watchedWriter`، `lookAt` | `engine/wedge.go:73-182` | نگهبان گیر |
| `guardSet` / `guards` / `run` / `lookAll` | `engine/wedge.go:185-280` | **یک goroutine سراسری** هر `guardTick`=۲s روی همهٔ نشست‌ها |
| `relayStream` | `engine/wedge.go:295-345` | رلهٔ کاربر با نگهبان و `relayDieGrace` |
| `tcpMemPressure`، `peerMemPressure`، `memPressure()` | `engine/mempressure.go:22-36` | فشار حافظهٔ TCP هر دو سرور |
| `refillHold`، `pickHeld`، `runRefill`، `refillStep`، `endRefill` | `engine/refill.go:62-351` | پخش اتصال‌ها هنگام پرشدن دوبارهٔ پول (goroutine هر اپیزود) |
| `dialGate`، `linkGate`، `acquireIf` | `engine/dialgate.go:28-77` | دروازهٔ dial سراسری |
| `burstLog` | `engine/burstlog.go:22-73` | جمع‌کردن خطوط لاگ پرتکرار |

### ۲.۴ goroutineهای کلیدی

| goroutine | کجا ساخته می‌شود | دوره |
|---|---|---|
| حلقهٔ پول (`Run`/`runAccept`) که `sampleHealth` و `decide` را می‌راند | `engine/linkmanager.go:567-582`، `1125-1139` | `healthTick` = ۲s |
| `openControl` (هر لینک) | `engine/stream_iran.go` در `OnLink`؛ `engine/control.go:72-…` | `controlInterval` = ۳s (لینک بیکار هر ۳ تا ۵ تیک؛ لینک «active نه heavy» با لرزش ۰ تا ۱ ثانیه) |
| `openStats`/`runStats` (هر لینک) | `engine/stats.go:106-198` | پول روی سیگنال `statsPoll` (فقط وقتی لینک ≥۱۶KiB در تیک جابه‌جا کرده) |
| `guardSet.run` (یکی در کل فرایند) | `engine/wedge.go:211`، `220-226` | `guardTick` = ۲s |
| `runRefill` (هر اپیزود) | `engine/refill.go:116-120`، `231-245` | `refillTick` = ۱۰۰ms |
| dialهای صف‌شده (`queueDial`) | `engine/linkmanager.go:908-949` | از دروازه |
| `tcpMemWatch.check` (دیمون) | `cmd/hs2/status.go:278`، `836-855` | `statusInterval` = ۲s (`cmd/hs2/status.go:29`) |

---

## ۳. جریان داده و کنترل، گام‌به‌گام

### ۳.۱ از سوکت تا `apSample` (هر تیک، `sampleHealth`)

1. **عکس بیرون از قفل** (`engine/linkmanager.go:1635-1665`): برای هر لینک زنده: `flowStats(now, dt, recentWin)`؛ از متر `rdBytes, wrBytes, wrBlocked`؛ `tcpStats`؛ `peerSeen`، `peerLoss`، `peer` (رکورد خروجی)، `statsState`، `ctrlWaitOf`، `ctrlAnsweredSent`؛ `wedged` اگر `guard.parkedAt` کمتر از `stuckWait` (۶s) پیش بوده.
2. **فشار حافظهٔ طرف مقابل** (`engine/linkmanager.go:1669-1677`): اگر رکورد تازه‌ای (≤ `statsStale`=۶s) با بیت `statsFlagMemPressure` باشد ⇒ `peerMemPressure=true`.
3. `dt` واقعی بین دو نمونه (حداقل ۱ms) و `perTick(b) = b × 2s / dt` (نرمال‌سازی به تیک ۲ ثانیه) (`engine/linkmanager.go:1621-1629`, `1682`).
4. برای هر لینک زیر قفل (`engine/linkmanager.go:1704-1905`):
   - `pickHist` جابه‌جا و `picks=0`؛ `flowing/open/recent` از `flowStats`.
   - **suspect**: اگر از آخرین دریافت ≥ `suspectAfter` (۱۲s) گذشته (`engine/linkmanager.go:1735-1744`).
   - اولین نمونه فقط پایه است (`pressed=false`، نرخ صفر، به `G` اضافه نمی‌شود ولی `flowing` اضافه می‌شود) (`engine/linkmanager.go:1745-1759`).
   - **نرخ**: `rate = (dRd+dWr)/secs`؛ `dom = max(dRd,dWr)/secs`؛ `rate10` = میانگین ۵ نرخ آخر (۱۰s)؛ `sustained` = کمینهٔ ۳ `dom` آخر (۶s) (`engine/linkmanager.go:1777-1790`). `goodput` (EWMA با `gpAlpha`=۰٫۴) فقط محاسبه می‌شود و هیچ‌جا خوانده نمی‌شود.
   - **فشار آپلود** (`engine/linkmanager.go:1797-1799`): `rawUp = perTick(dWr) ≥ 16KiB && dBlocked/dt ≥ 0.5 && upRwnd < 0.5`، که `upRwnd = Δrwnd_limited/Δbusy_time` (فقط اگر chrono معتبر). `upHist` سه بیت آخر.
   - **فشار دانلود** (`consumeRecord`, `engine/linkmanager.go:2032-2064`): هر رکورد تازهٔ خروجی (seq جدید) نسبت به قبلی: `perTick(tx) ≥ 16KiB && blk ≥ 0.5 && rwnd < 0.5` ⇒ بیت ۱ در `dnHist`. فاصلهٔ رکوردها > `statsGap` (۷s) یا شمارندهٔ عقب‌رفته ⇒ `dnHist=0` و فقط پایهٔ تازه. اگر `limited && !raw` ⇒ اشارهٔ rwnd (لاگ هر ۱۰ دقیقه).
   - **pressed** (`engine/linkmanager.go:1804-1806`): `up = popcount(upHist) ≥ 2`؛ `dn = statsOK && popcount(dnHist) ≥ 2 && now−lastRecAt ≤ 6s`؛ `pressed = serving() && (up || dn)`. نرخ `dom` لینک‌های pressed سالم در `pressedRates` (برای قاعدهٔ loss).
   - **poll آمار**: فقط اگر `perTick(dRd+dWr) ≥ 16KiB` (`engine/linkmanager.go:1812-1814`) ⇒ لینک بیکار ضرب‌آهنگ اضافه ندارد؛ پس دانلود pressed روی لینک تازه‌بیکارشده ۶ ثانیه بعد کهنه می‌شود.
   - loss و stuck (بخش ۵).
   - `s.G += rate`؛ `s.flowing += flowing`؛ `s.links = append(apLink())` (`engine/linkmanager.go:1902-1904`).
5. `growable = !accept || aged == 0 || poolOK > 0` — یعنی در معکوس، اگر همهٔ لینک‌های ≥۴ ثانیه‌ای pool-control را رد کرده باشند (خروجی قدیمی) ⇒ پول قابل رشد نیست (`engine/linkmanager.go:1995-2000`).
6. `m.sample = s` (`engine/linkmanager.go:2002`).

**نکتهٔ مهم:** `s.links` لینک‌های degraded/draining را هم دارد ولی با `serving=false, retiring=false` ⇒ در `S` و `R` شمرده نمی‌شوند؛ نرخشان در `G` هست. لینکی که بعد از عکس رسیده فقط با `serving/retiring/servingSince` می‌آید (`engine/linkmanager.go:1706-1710`).

### ۳.۲ `decide` مرحله‌به‌مرحله (`engine/autopilot.go:340-615`)

**پیش از مراحل — اندازه‌گیری‌های تیک:**

1. **قطعی کامل** (`len(s.links)==0`): T و تاریخچه دست‌نخورده، دلیل `"no link is up — waiting for links"` (`engine/autopilot.go:343-350`). آزمون: `TestAutopilotOutageKeepsSize`.
2. شمارش `S` (serving)، `R` (retiring)، `P` (serving و pressed)؛ برای هر لینک serving+pressed با `sustained>0` ⇒ `noteCap` (`engine/autopilot.go:353-367`).
3. `shortTick = P ≥ 1 && S−P < spare(P)` (`engine/autopilot.go:368`)؛ افزودن به `hist` (حداکثر ۳۰ خانه = ۶۰s).
4. `isShort` = در ≥۳ از ۵ تیک آخر short (`engine/autopilot.go:374-380`).
5. `fl5` = **کمینهٔ** `flowing` در ۵ تیک آخر (۱۰s)؛ `fl60` = **بیشینهٔ** `flowing` در ۶۰s؛ `p60` = بیشینهٔ `P` در ۶۰s؛ `shortIn60`؛ `gPeak` = بیشینهٔ میانگین دوتیکی G در ۶۰s («یک جهش تک‌تیکی قله نیست») (`engine/autopilot.go:381-409`).
6. `fGrow = ceil(fl5/perLink)`، `fHold = ceil(fl60/perLink)` (`engine/autopilot.go:410-411`).
7. `U = clamp(max(fl60+2, p60+spare(p60)))` — لینک‌هایی که جریان‌های فعال واقعاً می‌توانند به کار ببرند (`engine/autopilot.go:416`).
8. `cCap = capEstimate(now)`؛ `needBW = ceil(gPeak / (0.7·cCap))` اگر `cCap>0 && gPeak ≥ 16KiB/s` (`engine/autopilot.go:417-421`).
9. `needSat = p60 + spare(p60)`؛ `holdN = hold.n` اگر hold هنوز معتبر و `gPeak ≥ 0.6·hold.g` (`engine/autopilot.go:422-426`).
10. `need = min(max(needSat, needBW), U)`؛ سپس `max(need, holdN)` (hold به U محدود نمی‌شود)؛ سپس `max(need, fHold)`؛ **`H = clamp(need)`** = اندازه‌ای که تقاضای اخیر لازم دارد (`engine/autopilot.go:427-443`).
11. **بازنشانی عقب‌نشینی** اگر `k>0` (`engine/autopilot.go:448-461`): اگر confirm فعال است ⇒ هیچ؛ اگر میانگین G در ۱۰ تیک آخر (۲۰s) > `1.3·fail.g` **یا** `fl5 > int(1.5·fail.flows)+2` ⇒ `k=0` و `next = min(next, now+30s)`؛ وگرنه اگر ≥۳۰ دقیقه از شکست گذشته ⇒ `k=0`.
12. ساخت متن `why` (`engine/autopilot.go:463-467`).

**مراحل (اولین مرحله‌ای که return کند برنده است):**

| مرحله | شرط | اثر | فاز / لاگ |
|---|---|---|---|
| **۰ CONFIRM** (`470-500`) | `confirm.active`؛ اگر `T≠c.to` یا پروب در جریان ⇒ لغو | G را جمع می‌زند؛ در پایان پنجره (`confirmWin`=۶۰s): `need = max(c.need, gb + 2·√(varB/nb + varC/n))`؛ اگر میانگین < need ⇒ `T = from`، `k = kPrev+1`، ثبت `fail`، عقب‌نشینی؛ وگرنه `k=0` | `holding` / `pattern X → Y links: the gain after the last probe did not last …` |
| **۱ FLOOR** (`503-514`) | `floor = clamp(fGrow)`؛ در `!growable` سقف `S+R`؛ `floor > T` | `pr = nil` (لغو پروب)، `T = floor`، `lastGrowAt = now` — **پرش مستقیم بدون پروب** | `scaling` / `pattern X → Y links: N active connections (per_link P)` |
| **۲ پروب در جریان** (`517-519`) | `pr != nil` | `judge(...)` (بخش ۳.۳) | `probing` / … |
| **۳ RESTORE** (`522-543`) | `isShort && shrinkFrom > T && now−lastShrinkAt ≤ 60s` | `T = shrinkFrom`؛ `undos` در پنجرهٔ ۲ ساعت؛ `ttl = 10m << (len(undos)−1)` سقف ۲h؛ `hold = {T, gPeak, now+ttl}`؛ `lastGrowAt = now` | `scaling` / `pattern X → Y links: the shrink to X left links at their limit — undone, held for TTL` |
| **۴ GROW** (`548-579`) | `isShort && shortTick && growable && S ≥ T && T < U && T < max && now ≥ next && len(hist) ≥ 10` | `step = min(ceil(T/4), 32)`؛ اگر `chain>0` و `now−lastGrowAt ≤ 60s` ⇒ `step = min(ceil(T/2), 64)`؛ `to = min(T+step, U, max)`؛ پایه: میانگین/واریانس G در ۱۰ تیک آخر؛ `before[id] = rate10` لینک‌های retiring؛ `T = to` | `probing` / `pattern X → Y links (probe): P of S serving links at their limit, …` |
| **۵ SHRINK** (`582-603`) | `H < T` به مدت ≥ `shrinkDwell` (۶۰s، از `belowSince`) **و** (`!shortIn60` یا `T > U`) **و** ≥۳۰s از آخرین shrink **و** ≥۶۰s از آخرین grow | `step = max(1, ceil((T−H)/2))`؛ `T = max(T−step, H)`؛ `shrinkFrom = old` | `shrinking` / `pattern X → Y links: demand needs ~H — …; extra links take no new connections and close when theirs end` |
| **۶ HOLD/STEADY** (`606-614`) | `isShort && now < next` ⇒ holding؛ وگرنه steady | — | بدون note |

**`out`** (`engine/autopilot.go:834-841`): `T = clamp(T)`؛ اگر `!growable && T > S+R && S+R ≥ min` ⇒ `T = S+R`.

### ۳.۳ چرخهٔ عمر پروب (`judge`, `engine/autopilot.go:618-780`)

فرض: تیک N تصمیم GROW گرفت (T = to). در همان تیک `reconcile` اول لینک‌های retiring را un-retire می‌کند (پرمشغله‌ترین اول: بیشترین `open`، بعد تازه‌ترین `lastByte`؛ `engine/linkmanager.go:781-796`) و کسری را dial می‌کند (حداکثر `ceil(T/4)` در تیک، از دروازه؛ `engine/linkmanager.go:842-861`). در معکوس، خروجی از `kindPool` هدف تازه را می‌گیرد و dial می‌کند.

| تیک | رویداد | path:line |
|---|---|---|
| N+1… | **انتظار مسلح‌شدن**: اگر `S ≥ to` ⇒ `armed=true`، `aborts=0` (و همین تیک فقط «waiting» برمی‌گرداند). اگر `now−start > 15s + (to−from)×150ms` ⇒ **abort**: `T = from`، `chain=0`، `aborts++`، `next = now + 60s << (aborts−1)` (سقف ۸m، **بدون لرزش**)؛ `k` تغییر نمی‌کند | `622-645` |
| A (مسلح) +1، +2 | **نشست** (`settleTicks`=۲): اجازه به اتصال‌های تازه برای نشستن | `646-649` |
| A+3 … | **اندازه‌گیری** هر تیک: `newSum = Σ(rate − before[id])` و `probeSum = Σ rate` روی لینک‌های serving با `servingSince ≥ start`؛ `evalG += G`؛ «کوتاهی» روی لینک‌های **فعال** (`rate ≥ activeRate` = ۸KiB/s): `pAct ≥ 1 && act−pAct < spare(pAct)` ⇒ `evalShort++` | `650-676` |
| n ∈ {5,10,15} (A+7، A+12، A+17 = ۱۴، ۲۴، ۳۴ ثانیه پس از مسلح‌شدن) | **نگاه‌ها** (`looks`) | `677-687` |

آمار هر نگاه (`engine/autopilot.go:688-707`):
- `rNew = mean(evalNew)`، `pTot = mean(evalProbe)`، `relieved = 2·evalShort < n`
- `dG = mean(evalG) − gb`، `se = √(varB/nb + varA/n)`
- `rMin = max(32KiB/s, 0.25·gb/from)` (یک‌چهارم سهم هر لینک پایه)؛ `busy = pTot ≥ rMin`
- `need = max(0.5·rNew, 2.5·se, 0.05·gb)`

حکم‌ها (به همین ترتیب؛ `engine/autopilot.go:708-778`):

| حکم | شرط | اثر | متن note |
|---|---|---|---|
| **موفق** (additive) | `rNew ≥ rMin && dG ≥ need` (در هر نگاه) | `chain++`، `next = now+4s`، `lastGrowAt=now`، `startConfirm` (اگر شروع نشد ⇒ `k=0`) | `pattern F → T links kept: +X Mbit/s (new links carried Y)` |
| **relieved** (تقاضا تقریباً برآورده) | `rNew ≥ rMin && relieved && n==15 && dG > 0 && dG ≥ 0.05·gb` | `chain=0`، `next = now+30s`، `lastGrowAt=now`، `startConfirm` | `pattern F → T links kept as headroom: …` |
| **شکست** (substitutive، «مسیر پر») | (`(rNew ≥ rMin || busy) && n==15`) **یا** (`!relieved && rNew ≥ rMin && n==10 && dG < 0.25·rNew`) | `chain=0`، `T=from`، `k++`، `fail={now, max(gb,gPeak), fl60}`، `next = now+backoff()`؛ منطق **سقف** (پایین) | `sized to F links at ~X Mbit/s — N more links carried Y Mbit/s but the total rose only Z (path is full); next check in D` |
| **بی‌نتیجه** | `n==15` و هیچ‌کدام | `chain=0`، `next = now+30s`، **T در `to` می‌ماند** («spares»)، `lastGrowAt` تغییر **نمی‌کند** | `pattern F → T links kept as spares: no new connection reached them yet (connections stay on their link)` |

**سقف مسیر (`apCeil`)** در شکست (`engine/autopilot.go:755-767`):
- اگر سقف معتبر (`now−c.at < 1h`) و `from > c.n` و `gb ≤ 1.1·c.g` ⇒ **`T = c.n`** (برگشت به اندازهٔ کوچک‌تری که قبلاً همین throughput را داشت)، `c.at = now`؛ note: `pattern F → n links: F links carry no more than n did (~X Mbit/s; the path is full); next check in D`.
- اگر سقف نیست/منقضی/`gb > 1.1·c.g`/`from < c.n` ⇒ سقف تازه `{from, gb, now}`. وگرنه فقط `c.at = now`.

**دورهٔ آزمایشی (`startConfirm`)** (`engine/autopilot.go:800-811`): فقط اگر `k>0` یا شکست کمتر از ۳۰ دقیقه پیش. `need = gb + max(0.5·dG, 0.05·gb)`، `until = now+60s`، `next = until`، `chain = 0`. یعنی پس از اینکه مسیر یک‌بار پر دیده شد، هر سود تازه باید یک دقیقه دوام بیاورد و در این مدت پروب تازه نیست و زنجیره (گام ۵۰٪) خاموش است.

### ۳.۴ محرک (`reconcile`) — چه می‌کند با T

- **رشد**: اول retiring‌ها برمی‌گردند (`servingSince = now` ⇒ در پروب «لینک تازه» حساب می‌شوند و `before` نرخ قبلی‌شان را کم می‌کند)، بعد dial (`engine/linkmanager.go:781-796`, `842-861`).
- **کوچک‌سازی**: لینک‌های اضافی retiring می‌شوند به ترتیب: کمترین `flowing` → کمترین `recent` → کمترین `open` → کمترین `rate10` (`engine/linkmanager.go:797-816`). `pressed=false` برای retiring.
- **بستن**: `drainTick` فقط retiring‌ای را می‌بندد که `users==0 && Active()==0`؛ حداکثر `closesPerTick(R)` در تیک (`engine/linkmanager.go:983-988`, `1062-1064`)؛ اتصال‌های بیکار ≥ `drain_idle` (۳۱۰s) و پس از `retireForce` (۲۰ دقیقه) اتصال‌های «غیر flowing» با FIN بسته می‌شوند (`engine/linkmanager.go:1082-1117`).
- پس فاصلهٔ «T کاهش یافت» تا «لینک فیزیکی بسته شد» می‌تواند چند دقیقه باشد (آزمون‌ها: `TestSimTracksUpAndDown` ≤۸ لینک ۱۲ دقیقه پس از افت).

### ۳.۵ مسیر انتخاب لینک برای اتصال تازه (تعامل با `pressed`)

`pickKey` (`engine/linkmanager.go:1470-1494`): بی‌فشار اول؛ سپس کمترین `flowing + picks`؛ سپس کمترین `users`؛ تساوی کامل ⇒ تصادفی. `pressed` در کلید = `pressed || recentPicks ≥ max(1, perLink/2)` (۴ جاگذاری در ۳ نمونهٔ اخیر برای per_link=8) چون فشار «چند ثانیه دیر» اندازه‌گیری می‌شود. سطح‌ها: serving → retiring سالم → degraded/draining (`engine/linkmanager.go:1548-1586`). پس رشدِ autopilot فقط به جریان‌های **تازه** کمک می‌کند (اتصال‌ها سنجاق‌اند)؛ همین منطق در شبیه‌ساز تکرار شده (`engine/autopilot_sim_test.go:172-202`).

---

## ۴. جدول ثابت‌ها، آستانه‌ها، بافرها و زمان‌سنج‌ها

### ۴.۱ تنظیمات autopilot (`defaultTunables`, `engine/autopilot.go:126-167`)

| نام | مقدار | path:line | معنی |
|---|---|---|---|
| `tick` | `healthTick` = 2s | `autopilot.go:128` | دورهٔ تصمیم |
| `histTicks` | 30 (=60s) | `autopilot.go:129` | پنجرهٔ `fl60, p60, gPeak, shortIn60` |
| `shortWin`, `shortN` | 5, 3 | `autopilot.go:130-131` | کمبود = short در ≥۳ از ۵ تیک آخر |
| `armTimeout` | 15s | `autopilot.go:132` | مهلت بالا آمدن لینک‌های پروب (+ `armPerLink`) |
| `settleTicks` | 2 | `autopilot.go:133` | تیک‌های نشست پس از مسلح‌شدن |
| `looks` | {5, 10, 15} | `autopilot.go:134` | تیک‌های ارزیابی که حکم بررسی می‌شود |
| `baseTicks` | 10 (=20s) | `autopilot.go:135` | پایهٔ پروب؛ همچنین حداقل تاریخچه برای رشد |
| `z` | 2.5 | `autopilot.go:136` | حاشیهٔ نویز بر حسب خطای معیار |
| `additivity` | 0.5 | `autopilot.go:137` | کل باید ≥ نصف ترافیک لینک‌های تازه بالا برود |
| `minGain` | 0.05 | `autopilot.go:138` | … و ≥۵٪ پایه |
| `earlyFail` | 0.25 | `autopilot.go:139` | شکست زودهنگام در نگاه دوم اگر `dG < 0.25·rNew` |
| `rMinAbs` | 32 KiB/s (~262 kbit/s) | `autopilot.go:140` | حداقل ترافیک لینک‌های تازه برای حکم |
| `rMinFrac` | 0.25 | `autopilot.go:141` | … یا ۲۵٪ سهم پایهٔ هر لینک |
| `backoffBase` | 30s | `autopilot.go:142` | عقب‌نشینی پس از شکست اول |
| `backoffMax` | 8m | `autopilot.go:143` | سقف عقب‌نشینی (شکست و abort) |
| `jitter` | 0.2 | `autopilot.go:144` | ±۲۰٪ روی عقب‌نشینی شکست |
| `successNext` | 4s | `autopilot.go:145` | فاصلهٔ پروب بعدی پس از موفقیت |
| `inconclusiveNext` | 30s | `autopilot.go:146` | پس از بی‌نتیجه/relieved |
| `abortNext` | 60s | `autopilot.go:147` | abort اول (دوبرابر شونده) |
| `capWindow` | 30m | `autopilot.go:148` | عمر ورودی تخمین ظرفیت |
| `capMinSamples` | 6 | `autopilot.go:149` | حداقل لینک pressed برای اعتبار تخمین |
| `capMax` | 256 (یا `4·max` اگر بزرگ‌تر) | `autopilot.go:150`, `272-274` | سقف ورودی‌های `caps` |
| `util` | 0.7 | `autopilot.go:151` | لینک‌ها در قله حداکثر ۷۰٪ ظرفیت |
| `minBWForNeed` | 16 KiB/s (~131 kbit/s) | `autopilot.go:152` | زیر این قله، پهنای باند لینک اضافه توجیه نمی‌کند |
| `shrinkDwell` | 60s | `autopilot.go:153` | ماندن زیر هدف پیش از اولین کوچک‌سازی |
| `shrinkStep` | 30s | `autopilot.go:154` | فاصلهٔ دو گام کوچک‌سازی |
| `noShrinkAfterGrow` | 60s | `autopilot.go:155` | پس از رشد |
| `overshootWin` | 60s | `autopilot.go:156` | پنجرهٔ RESTORE پس از کوچک‌سازی |
| `holdBase` | 10m | `autopilot.go:157` | hold پس از اولین undo |
| `holdMax` | 2h | `autopilot.go:158` | سقف hold |
| `holdVoid` | 0.6 | `autopilot.go:159` | hold باطل اگر قله < ۶۰٪ قلهٔ زمان undo |
| `undoWindow` | 2h | `autopilot.go:160` | undoهای شمرده‌شده برای دوبرابر شدن |
| `kResetAfter` | 30m | `autopilot.go:161` | بازنشانی `k` پس از شکست |
| `chainWindow` | 60s | `autopilot.go:162` | موفقیتی به این تازگی ⇒ گام ۵۰٪ |
| `activeRate` | `pressMinBytes/2s` = 8 KiB/s | `autopilot.go:163` | لینک زیر آن برای «کوتاهی» داوری نمی‌شود |
| `confirmWin` | 60s | `autopilot.go:164` | دورهٔ آزمایشی |
| `ceilTTL` | 1h | `autopilot.go:165` | عمر سقف مسیر |

### ۴.۲ ثابت‌های دیگر autopilot

| نام | مقدار | path:line | معنی |
|---|---|---|---|
| `probeStepMax` | 32 | `autopilot.go:302` | سقف گام پروب عادی |
| `probeChainStepMax` | 64 | `autopilot.go:303` | سقف گام پروب زنجیره‌ای |
| `armPerLink` | 150ms | `autopilot.go:306` | مهلت اضافه برای هر لینک پروب (دروازه ~۱۰/ث) |
| `spare(p)` | `0` اگر p=0؛ وگرنه `min(ceil(p/4), max(4, ceil(p/16)))` | `autopilot.go:284-296` | p≤64: `min(ceil(p/4),4)`؛ مثال: 1→1، 4→1، 5→2، 9→3، 13→4، 64→4، 65→5، 100→7، 160→10، 300→19 |
| گام پروب | `min(ceil(T/4),32)`؛ زنجیره `min(ceil(T/2),64)` | `autopilot.go:552-555` | T=8: +2 (زنجیره +4)؛ T=100: +25 (+50)؛ T=200: +32 (+64) |
| عقب‌نشینی شکست | k=1..: 30s، 1m، 2m، 4m، 8m، 8m… ×[0.8,1.2] | `autopilot.go:784-791` | آزمون `TestProbeBackoffAndReset` |
| abort | 60s، 2m، 4m، 8m… (بدون لرزش) | `autopilot.go:633-637` | `aborts` با مسلح‌شدن صفر می‌شود |
| hold | 10m، 20m، 40m، 80m، 2h (سقف) | `autopilot.go:535-539` | آزمون `TestAutopilotOvershootRestore` |
| `T` اولیه | `warmSize(min,max)` = clamp(8) | `autopilot.go:276`, `linkmanager.go:742-751` | `SetWarm` می‌تواند بالاتر ببرد |

### ۴.۳ سیگنال‌های اندازه‌گیری (`health.go`، `linkmanager.go`)

| نام | مقدار | path:line | معنی |
|---|---|---|---|
| `healthTick` | 2s | `health.go:18` | تیک نمونه‌گیری |
| `gpAlpha` | 0.4 | `health.go:21` | EWMA goodput (فقط تشخیصی؛ خوانده نمی‌شود) |
| `activeBytes` | 96 KiB در تیک | `health.go:25` | دروازهٔ فعالیت برای داوری loss |
| `mss` | 1400 | `health.go:28` | مخرج پشتیبان (بایت/mss) |
| `lossFrac` | 0.12 | `health.go:31` | آستانهٔ loss |
| `degradeStreak` | 3 | `health.go:34` | نمونهٔ بد پیاپی |
| `flowTau` | 10s | `health.go:41` | ثابت زمانی EWMA هر جریان |
| `flowingRate` | 2 KiB/s (16 kbit/s) | `health.go:42` | آستانهٔ «flowing» |
| `flowRecent` | 6s | `health.go:43` | جریان باید در این مدت بایت جابه‌جا کرده باشد |
| `flowSteadyRate` | 256 B/s | `health.go:48` | ≥این در هر ۳ نمونهٔ آخر ⇒ flowing |
| `blockedMin` | 1ms | `health.go:53` | Write کوتاه‌تر = کار CPU، نه انتظار شبکه |
| `maxDrain` | 45s | `health.go:74` | پس از آن اتصال‌های بی‌داده بسته |
| `drainStall` | 15s | `health.go:75` | «بی‌داده» |
| `maxDrainActive` | 90s | `health.go:76` | سقف عمر لینک degraded |
| `warmStartLinks` | 8 | `health.go:85` | اندازهٔ شروع |
| `retireAfterDrop` | 4s | `health.go:90` | معکوس: صبر پیش از بستن پس از کاهش هدف |
| `controlInterval` | 3s | `health.go:95` | پینگ کنترل |
| `pressBlocked` | 0.5 | `linkmanager.go:70` | سهم زمان مسدود |
| `pressMinBytes` | 16 KiB در تیک | `linkmanager.go:71` | حداقل حجم برای pressed |
| `rwndShareMax` | 0.5 | `linkmanager.go:72` | سهم محدودیت پنجرهٔ گیرنده |
| `statsStale` | 6s | `linkmanager.go:75` | رکورد کهنه‌تر دیگر فشار نیست |
| `statsGap` | 7s | `linkmanager.go:76` | رکوردهای دورتر مقایسه نمی‌شوند |
| `suspectAfter` | 12s | `linkmanager.go:317` | بی‌دریافت ⇒ suspect |
| `pickWindow` | 3 | `linkmanager.go:1497` | نمونه‌هایی که جاگذاری‌شان در کلید pressed می‌آید |
| `drainHeadroom(n)` | `max(2, ceil(n/8))` | `linkmanager.go:1253` | سقف تخلیه/جایگزین همزمان؛ max=32→4، 48→6، 64→8، 300→38 |
| `closesPerTick(r)` | `min(max(2, ceil(r/32)), 8)` | `linkmanager.go:1062-1064` | بستن retiring خالی در تیک |
| `NotSentLowat` | 32 KiB | `tlscarrier/tune_linux.go:19` | پایهٔ معنایی `wrBlocked` (sendmsg پس از ~۳۲KiB صف منتظر می‌ماند) |
| `UserTimeoutMs` | 20000 | `tlscarrier/tune_linux.go:22` | TCP_USER_TIMEOUT |
| `SmuxStreamBuffer` / `SmuxSessionBuffer` / `SmuxFrameSize` | 2 MiB / 8 MiB / 16 KiB | `mtcp_link.go:258-264` | پنجره‌ها (مرتبط با wedge) |
| smux keepalive | 4–8s، timeout 24s | `mtcp_link.go:278-279` | |
| `ctrlPending` | 4 | `control.go:43` | حداکثر پینگ بی‌پاسخ |
| `ctrlBusyBytes` | 4 KiB | `control.go:47` | لینک «active» برای پینگ هر تیک و شاهد stuck |
| `poolCtlInterval` | 3s | `exit_pool.go:32` | تازه‌سازی هدف روی دو لینک قدیمی |

### ۴.۴ stuck و slow-path (`engine/stuck.go:61-95`)

| نام | مقدار | path:line | معنی |
|---|---|---|---|
| `stuckWait` | 6s | `stuck.go:62` | عمر قدیمی‌ترین پینگ بی‌پاسخ |
| `stuckStreak` | 2 | `stuck.go:63` | نمونهٔ پیاپی |
| `stuckPrompt` | 2s | `stuck.go:69` | «پاسخ سریع» (RTT آخر و انتظار فعلی) |
| `stuckRecover` | 30s | `stuck.go:75` | حداقل پنجرهٔ بازیابی پس از مسیر کند |
| `stuckRecoverMax` | 2m | `stuck.go:76` | حداکثر آن |
| `stuckMoveFloor` | 12 KiB در تیک (~۶KB/s) | `stuck.go:82` | زیر این، لینک منتظر حتماً throttled است |
| `stuckInflate` | 4 | `stuck.go:91` | RTT پاسخ‌دهنده‌ها ≥۴× معمول ⇒ مسیر شلوغ |
| `stuckInflateFloor` | 500ms | `stuck.go:92` | … و بالای این |
| `stuckBaseMins` | 10 | `stuck.go:93` | پنجرهٔ «زمان معمول» (کمینهٔ میانه‌های دقیقه‌ای) |
| `stuckBaseMinN` | 3 | `stuck.go:94` | حداقل پاسخ‌دهنده در تیک برای ثبت |

### ۴.۵ loss (`engine/loss.go:52-68`)

| نام | مقدار | path:line | معنی |
|---|---|---|---|
| `lossPathMin` | 4 | `loss.go:56` | زیر این تعداد لینک داوری‌شده/pressed، آزمون مسیر اعمال نمی‌شود |
| `lossWinMin` | 1s | `loss.go:60` | پنجرهٔ دانلود کوتاه‌تر باز می‌ماند |
| `lossQuietKeep` | 20s | `loss.go:64` | نمونهٔ آرام streak تازه را نگه می‌دارد |
| `lossKeepShare` | 0.5 | `loss.go:67` | لینک با ≥۵۰٪ نرخ میانهٔ pressedها نگه داشته می‌شود |

### ۴.۶ wedge guard (`engine/wedge.go:45-70`)

| نام | مقدار | path:line | معنی |
|---|---|---|---|
| `wedgeLooks` | 3 | `wedge.go:48` | نگاه پیاپی پارک ⇒ wedged (≥۴s) |
| `starveCalls` | 2048 | `wedge.go:52` | کمتر از این Read از نگاه قبل + بیرون از Read ⇒ پارک |
| `stuckFor` | 6s | `wedge.go:55` | رله در Write محلی و بی‌پیشرفت |
| `wedgeLogEvery` | 30s | `wedge.go:57` | خلاصهٔ لاگ |
| `guardTick` | 2s | `wedge.go:62` | دورهٔ نگاه |
| `relayDieGrace` | 5s | `wedge.go:68-70` | مهلت تخلیهٔ رله پس از مرگ نشست |
| لاگ «wedged empty» | هر 10m | `wedge.go:276` | |

### ۴.۷ refill، dial gate، burstlog، فشار حافظه

| نام | مقدار | path:line | معنی |
|---|---|---|---|
| `refillHoldMax` | 10s | `refill.go:50` | سقف اپیزود |
| `refillStall` | 3s | `refill.go:55` | بی‌لینک تازه ⇒ پایان |
| `refillTick` | 100ms | `refill.go:56` | |
| `refillRearm` | 1m | `refill.go:57` | پس از پایان limit/stalled اپیزود تازه نیست |
| `refillKeep` | 5m | `refill.go:58` | خلاصه در وضعیت |
| سقف hold | `max(per_link, ceil((users+queue)/T))` | `refill.go:141-145` | |
| `refillWhySlow` | «به‌موقع» اگر `up·10 ≥ (1+took·10)·6` (≥۶۰٪ آهنگ) | `refill.go:279-285` | |
| `gateInflight` | 8 | `dialgate.go:22` | handshake همزمان |
| `gatePerSec` | 10 | `dialgate.go:26` | آهنگ اسمی (برای متن لاگ) |
| `jitterGap` | 40 + U[0,120) ms (میانگین ~۱۰۰ms) | `linkmanager.go:394-396` | فاصلهٔ شروع‌ها |
| `closeJitter` | 50 + U[0,200) ms | `linkmanager.go:399-401` | فاصلهٔ بستن‌ها |
| `dialFailRun` | 3 | `linkmanager.go:865` | شکست پیاپی ⇒ رها کردن صف |
| `burstLines` / `burstWin` | 8 / 10s | `burstlog.go:18-19` | بیش از ۸ خط در ۱۰s ⇒ خلاصه |
| آستانهٔ فشار | `mem ≥ tcp_mem[1]` روشن، `mem < 0.9·tcp_mem[1]` خاموش | `cmd/hs2/status.go:845-853` | پسماند |
| `statusInterval` | 2s | `cmd/hs2/status.go:29` | بررسی `/proc/net/sockstat` |

---

## ۵. حلقه‌های کنترلی

| حلقه | ورودی | شرط | خروجی | دوره |
|---|---|---|---|---|
| **autopilot** | `apSample` | مراحل ۰–۶ | `T`، phase، reason، note | ۲s |
| **کف (floor)** | `fl5` (کمینهٔ flowing در ۱۰s) | `ceil(fl5/8) > T` | T بی‌درنگ بالا (لغو پروب) | ۲s |
| **رشد (probe)** | `P,S` و تاریخچه | بخش ۳.۲ | +۲۵٪ / +۵۰٪، حکم در ۱۴–۳۴s پس از مسلح‌شدن | ≥۴s پس از موفقیت؛ ≥۳۰s پس از بی‌نتیجه؛ عقب‌نشینی پس از شکست |
| **تخمین ظرفیت** | `sustained` لینک‌های serving+pressed | — | میانهٔ «بهترین هر لینک» در ۳۰ دقیقه (≥۶ لینک) ⇒ `needBW` | ۲s |
| **بازنشانی k** | میانگین G در ۲۰s، `fl5` | `> 1.3·fail.g` یا `fl5 > 1.5·fail.flows+2`؛ یا ۳۰ دقیقه | `k=0`، پروب ظرف ≤۳۰s | ۲s |
| **CONFIRM** | G در ۶۰s | میانگین < `need` | برگشت به `from` و `k = kPrev+1` | یک‌باره پس از موفقیت/relieved بعد از «مسیر پر» |
| **کوچک‌سازی** | `H` | ۶۰s زیر هدف، ۳۰s بین گام‌ها، ۶۰s پس از رشد، و بی‌کمبود در ۶۰s (مگر T>U) | `T −= ceil((T−H)/2)` | ۲s (اثر هر ۳۰s) |
| **RESTORE + hold** | کمبود پس از کوچک‌سازی | ≤۶۰s پس از کوچک‌سازی | T قبلی + hold ۱۰m×2^j | — |
| **فشار آپلود** | `wrBytes`, `wrBlocked`, TCP_INFO chrono | ≥16KiB، ≥50٪ مسدود، rwnd<50٪، ۲ از ۳ | `pressed` | ۲s |
| **فشار دانلود** | رکورد `kindStats` خروجی | همان، با رکورد ≤۶s و statsOK | `pressed` | یک رکورد برای هر poll (هر تیک پرمشغله) |
| **loss** | upload: TCP_INFO محلی؛ download: پنجرهٔ بین دو pong | >۱۲٪، ≥96KiB، ۳ نمونه، `calm`، نه `recovering` | degrade (≤`drainHeadroom`) یا «مسیر پراتلاف» | ۲s / ۳s |
| **stuck** | `ctrlWait`، جابه‌جایی تیک، پاسخ‌دهنده‌ها | ≥۶s، `moved < activeBytes`، ۲ نمونه، شاهد سالم، `moved < max(12KiB, نصف میانه)` | degrade+stuck | ۲s |
| **slow-path** | `waitingN`، `answering`، RTT معمول، فشار حافظه | `press || waitingN ≥ 2 && (waitingN > len(answering) || inflated)` | توقف loss و stuck تا `stuckRecoverFor` | ۲s |
| **wedge guard** | `rdCalls/inRead`، `wseq` رله‌ها | پارک ≥۳ نگاه و رلهٔ گیر ≥۶s؛ یا فشار حافظه و رلهٔ گیر | ریست (RST) رلهٔ گیر | ۲s |
| **فشار حافظه** | `/proc/net/sockstat`، بیت رکورد خروجی | `mem ≥ tcp_mem[1]` | `tcpMemPressure`/`peerMemPressure` | ۲s |
| **refill** | رسیدن لینک وقتی هیچ لینکی زنده نیست | `target ≥ 2` و خارج از `quietTill` | صف‌کردن اتصال‌ها با سقف fair share | ۱۰۰ms، ≤۱۰s |
| **dial gate** | هر dial لینک در فرایند | — | ≤۸ همزمان، فاصلهٔ ۴۰–۱۶۰ms | پیوسته |

### ۵.۱ جزئیات قاعدهٔ loss (`sampleHealth` + `lossVerdicts`)

- **آپلود** (`judgeUpLoss`, `engine/loss.go:98-107`): فقط اگر `tsOK && dWr ≥ 96KiB`؛ `segs = ΔData_segs_out` (لینوکس ≥۴٫۶) یا `dWr/1400 + dUp`؛ `frac = dUp/segs`.
- **دانلود** (`judgeDownLoss`, `engine/loss.go:115-138`): پنجره از pong قبلی تا pong فعلی؛ اولین pong یا شمارندهٔ عقب‌رفته ⇒ شروع پنجره؛ پنجرهٔ <۱s باز می‌ماند؛ اگر `dRd < 96KiB × win/2s` ⇒ «بسته ولی آرام»؛ `segs = ΔsegsIn + dR` (یا `dRd/1400 + dR`).
- streak هر جهت جدا؛ `!calm` (مسیر کند یا فشار حافظه در تیک قبل) ⇒ streak صفر؛ نمونهٔ آرام streak را تا ۲۰s از آخرین بد نگه می‌دارد (`engine/linkmanager.go:1832-1849`).
- نامزد: `streak ≥ 3 && bad` در همین تیک (`engine/linkmanager.go:1857-1859`).
- `lossVerdicts` (`engine/loss.go:180-225`): اگر ≥۴ لینک pressed ⇒ `pathRate` = میانهٔ `dom` آن‌ها، `keep = 0.5·pathRate`. اگر ≥۴ لینک داوری‌شده و **بیش از نصفشان** >۱۲٪ زیر `keep` ⇒ «مسیر پراتلاف»، هیچ‌کدام تخلیه نمی‌شود (لاگ دقیقه‌ای). وگرنه به ترتیب بیشترین loss، حداکثر `drainHeadroom(max) − degradedNow`، نامزدهایی که نرخ جهت پراتلافشان < `keep` است ⇒ `degraded=true, pressed=false`.
- اگر تیک فعلی `recovering` است حکمی صادر نمی‌شود (`engine/linkmanager.go:1948-1950`).

### ۵.۲ جزئیات stuck / slow-path (`engine/linkmanager.go:1864-1991`)

- `waits = !degraded && !draining && !wedged && ctrlWait ≥ 6s && moved < 96KiB` ⇒ `waitingN++`؛ اگر `!suspect` ⇒ `stuckStreak++` و در ≥۲ ⇒ نامزد.
- شاهد سالم (`answering`): `busy (moved ≥ 4KiB) && !degraded && !draining && !suspect && peerSeen && ctrlWait < 2s && rtt < 2s && ctrlAns ≠ 0`.
- `usual = promptFloor.base()` (کمینهٔ کمینه‌های دقیقه‌ای ۱۰ دقیقهٔ اخیر از میانهٔ پاسخ‌دهنده‌ها، وقتی ≥۳ پاسخ‌دهنده)؛ `inflated = usual>0 && len(answering) ≥ 3 && promptMed > max(500ms, 4·usual)`.
- `slow = press || waitingN ≥ 2 && (waitingN > len(answering) || inflated)` ⇒ `stuckSlowAt = now`، `stuckSlowFor += dt` (طلسم تازه اگر از پنجرهٔ قبلی گذشته باشد).
- حکم stuck فقط اگر `len(answering) > 0 && !recovering`: به ترتیب طولانی‌ترین انتظار؛ رد اگر `lastAns ≤ c.sent` (هیچ رفت‌وبرگشت کاملی پس از پینگ این لینک شروع نشده) یا `perTick ≥ max(12KiB, میانهٔ answeringMoved/2)` یا جای `drainHeadroom` پر است ⇒ `degraded, stuck = true`.
- لینک stuck بدون صبر `maxDrain` اتصال‌های بی‌دادهٔ ≥۱۵s را می‌بندد؛ بقیه حداکثر ۹۰s (`engine/linkmanager.go:1225-1236`).

### ۵.۳ جزئیات wedge guard (`engine/wedge.go:136-182`)

- `parked++` اگر خوانندهٔ smux (`watchConn`) الان در Read نیست **و** کمتر از ۲۰۴۸ Read از نگاه قبل داشته؛ `parkedAt = ctrlNow()` (برای stuck خوانده می‌شود).
- رله گیر = `wseq` فرد (در Write محلی) و `now − progress ≥ 6s` (progress فقط در نگاه‌ها تازه می‌شود ⇒ عملاً ۶ تا ۸ ثانیه).
- `wedged = parked ≥ 3`؛ `squeezed = !wedged && memPressure() && len(stuck)>0`؛ در هر دو، رله‌های گیر از نقشه حذف و در goroutine جدا `kill` می‌شوند (`SetLinger(0)` + `Close` ⇒ RST)؛ در wedged، `parked = 0`.
- `wedgedEmpty` = wedged ولی رلهٔ گیری نیست ⇒ لاگ ۱۰ دقیقه‌ای («UDP/TUN backlog or a slow panel dial»).
- آزمون‌ها: آزادسازی ۵ رلهٔ گیر و ادامهٔ رلهٔ سالم؛ رلهٔ تنهای مکث‌کرده آزاد نمی‌شود؛ خوانندهٔ آهسته (۱۶KiB/s) توقف را پنهان نمی‌کند (آزادسازی ≤۱۲s)؛ رله پس از مرگ نشست ≤`relayDieGrace` تمام می‌شود.

---

## ۶. حالت‌ها و گذارها، خطاها و بازیابی

### ۶.۱ فاز autopilot

```
          floor>T / restore                     grow (isShort ∧ shortTick ∧ …)
 steady ───────────────► scaling        steady ───────────────────────────► probing
   ▲  ▲                     │                ▲                                 │ judge
   │  └─────────────────────┘ (next tick)    ├── success / relieved / inconcl ─┤
   │                                          │                                 │ fail / abort / confirm-fail
   │  H<T for 60 s …                          │                                 ▼
   └──────────── shrinking ◄─────────────── steady                          holding (isShort ∧ now<next)
```

- فاز فقط برای نمایش است؛ وضعیت واقعی `pr`، `confirm`، `next`، `k`، `hold`، `shrinkFrom` است.
- پروب: `nil → (grow) → unarmed → armed → settle(2) → eval(1..15) → nil` با خروج‌های success/relieved/fail/inconclusive/abort یا لغو به‌وسیلهٔ FLOOR.

### ۶.۲ حالت لینک (از دید autopilot)

| حالت | تعریف | در `apLink` | path |
|---|---|---|---|
| serving | `!retiring && !degraded && !draining && alive && !suspect` | `serving=true` | `engine/linkmanager.go:299-301` |
| retiring | `retiring && alive` | `retiring=true` | `engine/linkmanager.go:2068` |
| suspect | ≥۱۲s بی‌دریافت | `serving=false, retiring=false` (اگر retiring نباشد) | `engine/linkmanager.go:1739-1743` |
| degraded / draining / stuck | قواعد loss/stuck | نه serving نه retiring | `engine/linkmanager.go:2094-2099` |
| pressed | فقط serving | `pressed` | `engine/linkmanager.go:1806` |

### ۶.۳ خطاها و بازیابی

- **قطعی کامل**: هیچ لینکی ⇒ T و تاریخچه ثابت (`engine/autopilot.go:343-350`). پس از برگشت اولین لینک، refill hold شروع می‌شود (`engine/refill.go:94-121`).
- **خروجی بدون pool control** (`growable=false`): کف و T به `S+R` محدود؛ کف هر تیک تکرار و لاگ نمی‌شود (`engine/autopilot.go:504-506`, `836-838`؛ آزمون `TestAutopilotFloorNotRepeatedWhenNotGrowable`)؛ لاگ یک‌باره `the exit has no pool control (older hs2) …` (`engine/linkmanager.go:2017-2019`).
- **خروجی بدون kindStats** (قدیمی): `statsUnsupported` ⇒ فشار دانلود هرگز ⇒ رشد فقط از کف (آزمون‌ها: `probes == 0`)؛ لاگ `the other server does not report link stats (older hs2) …` (`engine/stats.go:158-161`).
- **لینک‌های پروب بالا نمی‌آیند** (خروجی به max خودش رسیده یا dial شکست): abort با عقب‌نشینی ۶۰s دوبرابرشونده (`engine/autopilot.go:626-643`).
- **کوچک‌سازی بیش از حد**: RESTORE فوری با un-retire (بی‌dial) + hold.
- **شمارندهٔ عقب‌رفتهٔ رکورد/pong** (خروجی ری‌استارت شد): پایهٔ تازه (`engine/linkmanager.go:2041-2049`، `engine/loss.go:120-123`).

---

## ۷. پیام‌های پروتکل مرتبط

(جزئیات کامل در `04-linkmanager.md`؛ این‌ها ورودی‌های autopilot/سلامت‌اند.)

- **kindStats** (`engine/stats.go:24-35`): لبه `[ver=1]` ← خروجی `[ver][recLen][caps]`؛ هر poll: لبه `[seq u32]` ← رکورد ۶۴ بایتی: `0 seq u32 | 4 flags u16 (bit0 chrono, bit1 tcp_info, bit2 mem pressure) | 6 reserved | 8 monoNs | 16 txBytes | 24 txBlockedNs | 32 busyUs | 40 rwndLimUs | 48 sndbufLimUs | 56 deliveryRate`. فیلدهای اضافهٔ آینده نادیده گرفته می‌شوند. **`sndbuf` و `delivery` ارسال می‌شوند ولی لبه از آن‌ها استفاده نمی‌کند.**
- **kindCtrl** (`engine/control.go:33-35`): ping `[seq:8][edgeNanos:8]`، pong `[seq:8][edgeNanos:8][exitRetrans:8]`.
- **kindPool** (`engine/exit_pool.go:29-33`): هر ۲ بایت یک عدد big-endian = `ctlTarget()` = `T + draining-being-replaced` (`engine/linkmanager.go:1340-1350`)؛ بر تغییر، و دوره‌ای (۳s روی دو لینک قدیمی، `poolCtlSlow` روی بقیه).

---

## ۸. متن دقیق لاگ‌های مهم

### ۸.۱ autopilot (note ها؛ با پیشوند `mtcp: ` و در معکوس شاید با `capNote`)

| متن (format) | path:line | معنی |
|---|---|---|
| `pattern %d → %d links: %d active connections (per_link %d)` | `autopilot.go:513` | رشد از کف |
| `pattern %d → %d links (probe): %d of %d serving links at their limit, %s` | `autopilot.go:577` | شروع پروب |
| `pattern %d → %d links kept: +%.1f Mbit/s (new links carried %.1f)` | `autopilot.go:719` | پروب موفق |
| `pattern %d → %d links kept as headroom: new links carried %.1f Mbit/s and no link is at its limit any more (total %+.1f)` | `autopilot.go:732-733` | relieved |
| `sized to %d links at ~%.1f Mbit/s — %d more links carried %.1f Mbit/s but the total rose only %.1f (path is full); next check in %s` | `autopilot.go:769-770` | پروب شکست |
| `pattern %d → %d links: %d links carry no more than %d did (~%.1f Mbit/s; the path is full); next check in %s` | `autopilot.go:761-762` | برگشت به سقف مسیر |
| `pattern %d → %d links kept as spares: no new connection reached them yet (connections stay on their link)` | `autopilot.go:777` | بی‌نتیجه |
| `pattern back to %d links: wanted %d but only %d came up in %s (peer not dialing or dials failing); retry in %s` | `autopilot.go:641-642` | abort |
| `pattern %d → %d links: the gain after the last probe did not last (%.1f Mbit/s over the next minute, %.1f needed) — the path is full; next check in %s` | `autopilot.go:494-495` | confirm شکست |
| `pattern %d → %d links: the shrink to %d left links at their limit — undone, held for %s` | `autopilot.go:542` | RESTORE |
| `pattern %d → %d links: demand needs ~%d — %s; extra links take no new connections and close when theirs end` | `autopilot.go:602` | کوچک‌سازی |
| ` — capped at %d by the Kharej server (its max_links), so at most %d links run` | `linkmanager.go:618` | پسوند فقط نمایشی در معکوس |

متن `why`: `%d active of %d open connections, %d of %d serving links at their limit, peak %.1f Mbit/s` + ` (one link carries ~%.1f Mbit/s)` (`autopilot.go:463-467`).
reasonهای بدون note (مانیتور): `no link is up — waiting for links`، `trying %d links — waiting for them to come up (%d up)`، `trying %d links — letting new connections land`، `trying %d links — measuring (%d/%d)`، `links at their limit, but %s; next check in %s` (با `waitWhy`)، `sized for current demand: …`، `demand needs ~%d; stepping down after a steady minute — …`.

### ۸.۲ سلامت

| متن | path:line |
|---|---|
| `link %d: nothing received for %s — not used for new connections until it answers` | `linkmanager.go:1742` |
| `link %d degraded (up-loss %v, down-loss %v%s, rtt %dms) — draining` (`%s` = `, moving X Mbit/s where the busy links get Y`) | `loss.go:221-222` |
| `%d of %d busy links resend more than %.0f%% — the path is lossy, not those links: none is drained` | `loss.go:202-203` |
| `link %d stuck: its traffic has waited %s for an answer while it moved %s in %s (the other links answer in ~%dms) — draining` | `linkmanager.go:1988-1989` |
| `%d of %d busy links have waited %s+ for an answer and only %d answer promptly — the path or the other server is slow, not those links: none is drained` | `linkmanager.go:1961-1962` |
| `%d of %d busy links have waited %s+ for an answer and the %d that answer promptly take ~%dms, %.0f× their usual ~%dms — the path is congested, not those links: none is drained` | `linkmanager.go:1964-1965` |
| `kernel TCP memory on %s is above its pressure mark — every socket there is squeezed, not the links: none is judged (look for stalled readers)` | `linkmanager.go:1957` |
| `downloads are limited by this server's receive side (a slow reader or small tcp_rmem), not the path — more links would not help` | `linkmanager.go:2022` (هر ۱۰ دقیقه) |
| `mtcp: reset %d connection(s) on %d link(s) whose app had taken nothing for %s while the link's receive buffer was full — the links' other connections keep flowing` | `wedge.go:272-273` |
| `mtcp: reset %d connection(s) whose app had taken nothing for %s while kernel TCP memory was above its pressure mark (here or on the other server) — their buffers squeezed every socket` | `wedge.go:267-268` |
| `mtcp: %d link(s) stopped reading for several seconds with no stuck connection to release (UDP/TUN backlog or a slow panel dial)` | `wedge.go:278` |
| `kernel TCP memory: %d MB in TCP buffers, above the kernel's pressure mark (%d MB of %d MB, tcp_mem) — …` / `kernel TCP memory: back below the pressure mark (%d MB of %d MB)` | `cmd/hs2/status.go:847`, `851` |

### ۸.۳ refill و burstlog

| متن | path:line |
|---|---|
| `refill: %d of %d links up — new connections wait (%s at most) for a link with room, so they spread over the links still opening instead of piling onto the first ones: at most %d open connections per link (the fair share) until the pool is up` | `refill.go:183-184` |
| `refill: %s up after %s — %d connection(s) waited for a link with room (longest %s); at most %d open connections on one link (the hold allowed %d)` | `refill.go:332-333` |
| `refill: no new link for %s with %d of %d links up — the other server keeps no more …` | `refill.go:335-336` |
| `refill: hold ended at its %s limit with only %d of %d links up (%s) — the %d connection(s) still waiting went onto the existing links, none refused: …` | `refill.go:338-339` |
| `refill: hold ended at its %s limit with %d of %d links up — %d connection(s) waited …` | `refill.go:341-342` |
| `links open at the dial pace, about %d a second, so %d take about %ds — expected, not a fault` / `links are coming slower than the dial pace of about %d a second: a slow or lossy path, or the other server still starting` | `refill.go:282`, `284` |
| `%s+%d more %s in the last %s (latest: %s)` (مثلاً `mtcp: +K more links up in the last 10s (latest: …)`) | `burstlog.go:71` |

---

## ۹. گزینه‌های پیکربندی و متغیرهای محیطی

| گزینه | پیش‌فرض | اثر | path:line |
|---|---|---|---|
| `min_links` | 2 | `a.min` (کف مطلق) | `cmd/hs2/main.go:65`, `644-651` |
| `max_links` | غایب=32؛ 0=خودکار (۱ لینک/48MB، حداکثر 300)؛ عدد=ثابت | `a.max`؛ `capMax = max(256, 4·max)`؛ `drainHeadroom` | `cmd/hs2/main.go:66-70`, `711-…` |
| `per_link` | 8 | `perLink`: کف = `ceil(flowing/per_link)`؛ `fHold`؛ سقف refill؛ آستانهٔ `recentPicks` | `cmd/hs2/main.go:72`, `653-655` |
| `drain_idle_sec` | غایب=310s؛ ≤0=هرگز | بستن اتصال‌های بیکار روی retiring؛ همچنین پنجرهٔ `recent` | `cmd/hs2/main.go:73-76`, `768-777` |
| فایل warm (`<config>.warm`) | فقط اگر ≤15 دقیقه و فقط بالاتر از `warmSize` | `SetWarm` ⇒ `ap.T` و هدف اولیه | `cmd/hs2/status.go:192-236`، `cmd/hs2/main.go:280-293`، `engine/linkmanager.go:354-362` |
| `HS2_TUNE_NOTSENT` | 32768 | `TCP_NOTSENT_LOWAT` — معنای `wrBlocked` (فشار آپلود) به آن وابسته است | `cmd/hs2/main.go:267` |
| `HS2_TUNE_SMUX_STREAMBUF` / `HS2_TUNE_SMUX_SESSBUF` / `HS2_TUNE_SMUX_FRAME` | 2MiB / 8MiB / 16KiB | اندازهٔ بافرهای مربوط به wedge | `cmd/hs2/main.go:268-270` |
| `HS2_TUNE_CC` | bbr (پروفایل) | کنترل ازدحام | `cmd/hs2/main.go:271-274` |

**هیچ‌کدام از `apTunables`، آستانه‌های loss/stuck/wedge یا dial gate از پیکربندی یا محیط قابل تنظیم نیستند** (همه ثابت؛ برخی `var` فقط برای آزمون: `guardTick`، `relayDieGrace`، `bornSpareGrace`، `refill*`، `linkGate`، `statsReopenAfter`).

---

## ۱۰. آزمون‌ها: چه چیزی تضمین شده

### ۱۰.۱ شبیه‌ساز (`engine/autopilot_sim_test.go`)
گیاه جریان‌سطح: اتصال‌های سنجاق‌شده، سقف هر لینک (throttle هر اتصال) و سقف مسیر با تقسیم max-min عادلانه، اتصال بیکار با بستن پنل پس از ۳۰۰s، همان ترتیب pick و victim، تأخیر dial، و در معکوس خروجی‌ای که slot نگه می‌دارد. فشار = تقاضا > تخصیص×۱٫۰۲ و ≥ `pressMinBytes`؛ ۲ از ۳ با یک تیک تأخیر (`engine/autopilot_sim_test.go:369-371`, `445-448`). autopilot همان کد واقعی است.

### ۱۰.۲ `engine/autopilot_test.go`
| آزمون | تضمین |
|---|---|
| `TestSimProductionReplay` (capped video / no cap) | ترافیک واقعی (۲۵۰ بیکار، ۱۷ فعال، یک ویدئو ۳Mbit/s): T در ۳ دقیقه ≤۵، هرگز >۸، **صفر پروب**، صفر قطع، همیشه reason |
| `TestSimProductionReplayFromStuck32` | از ۳۲ پایین می‌آید (≤۶ در ۴ دقیقه، ≤۷ لینک در ۱۲ دقیقه)، بدون قطع اتصال فعال |
| `TestSimTracksUpAndDown` | تقاضای ۶→۲۵→۶ Mbit/s با throttle ۲Mbit/s: ≥۸۲٪ (میانگین ≥۸۸٪) تقاضا و T≥۱۲ در اوج؛ T در نزول بالا نمی‌رود؛ ≤۹ شش دقیقه بعد؛ ≤۸ لینک ۱۲ دقیقه بعد؛ بدون kindStats: صفر پروب، maxT≤۸ |
| `TestSimNoGrowthUnderBurstyNoise` | ترافیک انفجاری بدون سقف ⇒ صفر پروب |
| `TestSimPathFullNoRatchet` | مسیر پر (۸Mbit/s): صفر موفقیت، ≤۲۰ پروب در ۲ ساعت، k به ≥۵ می‌رسد، T بالا نمی‌خزد |
| `TestSimConstantPressedNoCreep` | فشار ثابت روی مسیر پر: T(۱۰m) = T(۳۰m) |
| `TestSimSinglePinnedCappedFlowNoProbe` | یک جریان capped سنجاق‌شده (S=۲..۸) هرگز پروب نمی‌سازد |
| `TestSimInconclusiveKeepsSpare` | پروبی که هیچ جریان تازه‌ای به آن نرسید: بی‌نتیجه، T=۴ نگه داشته، تکرار نمی‌شود، dial اضافه نیست |
| `TestSimPerFlowGrowthFollowsArrivals` | ۲۴ جریان bulk با throttle ۱Mbit/s: ≥۲۲ روی لینک خودشان در ۳ دقیقه |
| `TestSimReconnectStormIgnored` | ۳۰۰ اتصال handshake در ۱۰s ⇒ رشد نیست |
| `TestSimSevereThrottling` | ۴۰۰kbit/s هر اتصال: ≥۶۰ از ۶۴ جریان flowing شمرده می‌شوند و T≥۱۶ |
| `TestAutopilotOvershootRestore` | undo فوری، hold ۱۰/۲۰/۴۰ دقیقه، ۲ تا ۳ undo در ساعت |
| `TestSimOscillationBound` | موج مربعی ۶۰/۱۲۰s: ≤۶ تغییر هدف و ≤۲ بستن در هر ۱۰ دقیقه |
| `TestSimPropertyIdleSettlesAtMin` | ۲۰۰ تاریخچهٔ تصادفی: پس از توقف ترافیک دقیقاً `min` |
| `TestSimProbeArmsOnlyWhenLinksUp` | خروجی dial نمی‌کند ⇒ ≥۲ abort، بدون حکم، فاصلهٔ ≥۶۰s بین abort و پروب بعدی، T به ۸ برمی‌گردد |
| `TestAutopilotOutageKeepsSize` | قطعی ۱۰ دقیقه‌ای: T ثابت و تاریخچه دست‌نخورده |
| `TestAutopilotFloorNotRepeatedWhenNotGrowable` | بدون pool control: T≤۴ و حداکثر یک note |
| `TestSimEnvelope` | تقاضای نامحدود ≤ max؛ بیکاری ⇒ دقیقاً min |
| `TestProbeVerdictTable` | additive⇒success، substitutive⇒fail، relieved+سود⇒relieved، relieved بی‌سود⇒fail، بی‌ترافیک⇒inconclusive |
| `TestProbeVerdictNoiseFalsePass` | با نویز CV ۰٫۳، نرخ قبول نادرست < ۳٪ (۱۰٬۰۰۰ دانه) |
| `TestProbeBackoffAndReset` | ۳۰s، ۱m، … سقف ۸m ±۲۰٪؛ بازنشانی با تقاضای ۲× یا ۳۰ دقیقه |
| `TestProbeUnretiredBusyLinksWithoutGainFails` | لینک‌های برگشته از retiring که ترافیک قدیمی دارند و سودی نیست ⇒ fail (نه spare) |
| `TestBackoffNotResetBySpike` | جهش ۴ ثانیه‌ای ۱٫۴× بازنشانی نمی‌کند؛ ۱٫۵× پایدار می‌کند |
| `TestAutopilotNoShrinkRestoreFlap` | ۹ لینک pressed با جریان‌های زیر آستانه: بدون نوسان shrink/restore |
| `TestSimNoisyFullPathNoDrift` | مسیر پر با ±۲۰٪ نویز به مدت ۸ ساعت: T≤۱۰، maxT≤۱۷ |
| `TestCapEstimateNotDraggedBySlowLinks` | دو لینک کند ۱۰ دقیقه تخمین ۵Mbit/s را پایین نمی‌کشند؛ بهترین هر لینک حفظ؛ پس از پنجره ⇒ ۰ |

### ۱۰.۳ `engine/autopilot_scale_test.go`
| آزمون | تضمین |
|---|---|
| `TestAutopilotScaledParamsMatchSmallPools` | `spare` تا ۶۴ دقیقاً فرمول قدیمی؛ 65→5، 160→10، 300→19؛ `armTimeoutFor(64,80)=15s+16×150ms`؛ `capMax` 256 / 1200 |
| `TestSimPressureGrowthReachesHundreds` (غیرکوتاه) | ۶۰۰ جریان ۱Mbit/s با throttle ۲Mbit/s: maxT ≥۲۱۰ و ≥۵۵٪ تقاضا (مستقیم و معکوس) |
| `TestClosesPerTickScales` | ۲ تا ۶۴، یک‌سی‌ودوم بالاتر، سقف ۸ |
| `TestReclaimForcedClosesTricklingNotFlowing` | پس از `retireForce` فقط اتصال‌های غیر flowing بسته |

### ۱۰.۴ سلامت
- `health_test.go`: `TestPickSkipsDegraded`، `TestDegradeAndHeal` (۲۰٪ loss ⇒ degrade، یک dial جایگزین، بستن پس از خالی شدن)، `TestNoDegradeIdleLink` (زیر `activeBytes`)، `TestDegradeOnDownloadLoss`، `TestNoDegradeHealthy`.
- `loss_test.go`: `TestDownLossFollowsThePongWindow` (۳۰٪ با pong منظم تخلیه؛ ۹٪ با pong لرزان نه؛ ۲۰٪ لرزان تخلیه)، `TestDownLossShortWindowStaysOpen`، `TestUpLossCountsSegments` (۷٪ قطعه نه، ۲۰٪ بله)، `TestLossPathWideDrainsNone` (۷ از ۱۰ با ۱۵٪ ⇒ هیچ؛ لاگ ۱–۲ بار در ۸۰s)، `TestLossFindsTheLossyAmongThrottledLinks`، `TestLossDrainsAtMostHeadroom`، `TestLossFewBusyLinksJudgedAlone`، `TestLossKeepsALinkAtThePathsRate`، `TestLossStreakSurvivesShortQuietSamples`.
- `stuck_test.go` (خارج از فهرست ولی مرتبط): `TestStuckLinkIsDegraded`، `TestStuckNotFlagged`، `TestStuckLinkDrainsAtOnce`، `TestStuckDrainsAtMostHeadroomAtOnce`، `TestStuckMinorityIsCaught`، `TestStuckWaitsOutRecoveryAfterMassWait`، `TestStuckRecoveryGrowsWithTheSlowSpell`، `TestStuckBlipsDoNotStretchTheWindow`، `TestStuckSeparateSpellStartsAfresh`، `TestLossWaitsOutASlowSpell`، `TestSlowAnswersAreNotASlowPath`، `TestStuckShareIsHalfTheMedian`، `TestStuckFloorCatchesAThrottleAtNight`، `TestSlowPathNeedsWaitingLightLinks`، `TestStuckCongestedPathIsSlow`، `TestRTTFloorWindow`، و آزمون‌های کانال کنترل.
- `pool_v2_test.go` (فشار): `TestSampleHealthUploadPressure` (۱۶KiB دقیق، ۰٫۸ مسدود، rwnd ۰٫۶ رد، بدون chrono استثنای rwnd خاموش)، `TestSampleHealthUploadPressureTwoOfThree`، `TestSampleHealthDownloadPressure`، `…TwoOfThree` (retiring هرگز pressed)، `TestSampleHealthRwndHintLoggedOnce`، `TestSampleHealthStaleRecordNotPressed` (۶s)، `TestSampleHealthUnsupportedStatsNotPressed`، `TestSampleHealthRecordGapRebaselines` (۷s)، `TestFlowStatsFlowingThresholds` (handshake ۴KiB و heartbeat هرگز flowing).
- `measure_test.go`: `TestMeteredConnBlockedTime` (~۰ وقتی طرف مقابل می‌گیرد، بیشتر زمان وقتی کند است)، `TestFlowStatsFlowingNeedsSustainedRate`.

### ۱۰.۵ نگهبان‌ها
- `wedge_test.go`: `TestWedgeGuardReleasesStuckReaders`، `TestWedgeGuardSparesLonePausedReader`، `TestWedgeGuardTricklingReaderDoesNotHideStall` (غیرکوتاه؛ ≤۱۲s)، `TestRelayEndsWhenStreamDies`.
- `mempressure_test.go`: `TestWedgeGuardEndsPausedReaderUnderMemoryPressure`، `…UnderPeerMemoryPressure`، `TestNoVerdictsUnderMemoryPressure` (حکمی نیست تا پایان فشار + `stuckRecover`)، `TestPeerMemoryPressureFromStatsRecords` (کهنه شدن رکورد)، `TestStatsRecordCarriesMemoryPressure`.
- `refill_test.go`: `TestRefillHoldSpreadsAfterOutage` (۲۴۰۰ اتصال/۳۰۰ لینک: پرترین ≤۳۲، بدون رد)، `TestRefillHoldProductionRestart` (۶۰۰۰/۶۳: ≤۹۶)، `TestRefillHoldSlowRefillNeverRefuses`، `TestRefillHoldOnlyAfterStartOrTotalLoss`، `TestRefillCap`، `TestRefillHoldCancel`، `TestPickWaitRefillHoldLive` (UDP نگه داشته نمی‌شود)، `TestRefillWhySlowTellsPaceFromPath`، `TestRefillHoldReverseStopsAtKharejCeiling`، `TestRefillHoldEndsWhenLinksStopComing`.
- `dialgate_test.go`: `TestDialGatePacesAndBounds` (≤حد همزمانی، فاصله ≥gap)، `TestDialGateCancel`.

---

## ۱۱. «از قبل وجود دارد» (برای جلوگیری از دوباره‌کاری)

1. کنترلر خالص و قابل‌شبیه‌سازی با شبیه‌ساز جریان‌سطح و آزمون‌های سناریو (`engine/autopilot.go:11-15`، `engine/autopilot_sim_test.go`).
2. کف از جریان‌های **واقعاً فعال** (EWMA ۱۰ ثانیه، ۲KiB/s، تازگی ۶s، یا ۲۵۶B/s پیوسته) با per_link قابل تنظیم؛ اتصال‌های بیکار پنل شمرده نمی‌شوند.
3. رشد **فقط با پروب آزمون‌شده** و تفکیک «افزایشی» از «جانشینی» با آمار (z=۲٫۵، نصف ترافیک تازه، ۵٪ پایه)؛ حکم زودهنگام شکست؛ حالت relieved و spares.
4. گام پروب متناسب (۲۵٪) و زنجیره‌ای (۵۰٪) با سقف ۳۲/۶۴ لینک؛ مهلت مسلح‌شدن متناسب با گام.
5. عقب‌نشینی نمایی با لرزش، بازنشانی با رشد **پایدار** تقاضا (نه جهش)، و بازنشانی ۳۰ دقیقه‌ای.
6. سقف مسیر (`apCeil`) و دورهٔ آزمایشی (`confirm`) ضد «خزش» روی مسیر پر پرنویز.
7. `spare(p)` مقیاس‌پذیر برای صدها لینک.
8. کوچک‌سازی تقاضامحور با `H` (فشار اخیر، قلهٔ پهنای باند / ظرفیت×۰٫۷، کف ۶۰ ثانیه‌ای)، محدود به `U`، گام نصفِ فاصله، و undo فوری + hold دوبرابرشونده که با افت تقاضا باطل می‌شود.
9. تخمین ظرفیت **هر لینک** (بهترین نرخ پایدار در حال pressed، میانه، ≥۶ لینک، ۳۰ دقیقه).
10. سیگنال فشار دوجهته: آپلود محلی (`wrBlocked` + TCP_INFO chrono) و دانلود از رکورد خروجی؛ استثنای rwnd (گیرندهٔ کند ≠ مسیر) و اشارهٔ تنظیم `tcp_rmem`؛ چسبندگی ۲ از ۳.
11. retiring به‌جای بستن (هرگز اتصال در حال کار قطع نمی‌شود)، un-retire پیش از dial، ترتیب قربانی بر اساس زودتر-خالی-شدن، بستن تدریجی، drain_idle و retireForce.
12. پشتیبانی کامل معکوس: همان autopilot روی لبه، `kindPool`، born-spare، churn guard، `growable`.
13. warm start (هدف قبلی تا ۱۵ دقیقه) و refill hold با سقف fair share ثابت.
14. dial gate سراسری (۸ همزمان، ~۱۰/ث) با `acquireIf` (دادن نوبت به dialهایی که هنوز لازم‌اند).
15. قواعد سلامت: loss دوجهته با شمارش قطعه، پنجرهٔ pong، استثنای نرخ مسیر و «مسیر پراتلاف»، سقف `drainHeadroom`؛ stuck با شاهد رفت‌وبرگشت کامل؛ تشخیص مسیر کند/شلوغ با RTT معمول؛ پنجرهٔ بازیابی متناسب با طول طلسم؛ suspect ۱۲ ثانیه.
16. wedge guard بدون syscall و آزادسازی هدفمند (RST)؛ حالت فشار حافظهٔ TCP (محلی و از طرف مقابل) که هم guard را تهاجمی‌تر و هم قضاوت‌ها را خاموش می‌کند.
17. تخلیهٔ مرحله‌ای degraded (۴۵/۱۵/۹۰ ثانیه) و make-before-break با سقف `drainHeadroom`.
18. burstLog برای خوانایی لاگ در صدها لینک.
19. نمایش زنده: phase، reason، `CapMbit`، `PeakMbit`، `NextProbeS`، `Pressed`، `Saturated` (`engine/linkmanager.go:2216-2309`).

---

## ۱۲. ایده‌هایی که امتحان و رد شده‌اند (طبق کد/مستندات)

| ایده/نسخهٔ قبلی | چرا رد شد | مرجع |
|---|---|---|
| نسخهٔ اول autopilot با ورودی‌های قفل‌شونده | به ۳۲ چرخ‌دنده‌ای بالا رفت و پایین نیامد | `engine/autopilot.go:41-44` |
| تخمین ظرفیت از میانهٔ **نمونه‌ها** | در بار کم لینک‌های کند هر تیک نمونه می‌افزودند ⇒ تخمین ~۰٫۹Mbit/s ⇒ ۷۹Mbit/s قله «۴۸ لینک لازم» با ۱۳۷ کاربر، پول کوچک نشد | `engine/autopilot.go:843-849`، `CHANGELOG.md:1070-1074` |
| `spare` ثابت ۴ | در صدها لینک رشد در ~۱۶۵ (مستقیم) / ~۱۹۷ (معکوس) گیر کرد با ~نصف تقاضا | `engine/autopilot.go:279-283`، `engine/autopilot_scale_test.go:40-44` |
| حکم «spare» برای لینک‌های برگشته از retiring که ترافیک قدیمی دارند | پول روی مسیر پر می‌خزید | `engine/autopilot_test.go:753-757`، `engine/autopilot.go:734-741` |
| پذیرفتن هر سود پروب روی مسیر پر پرنویز | داور به ۳۲ رسید (drift) | `engine/autopilot_test.go:855-860` |
| بازنشانی عقب‌نشینی با یک قله | تخلیهٔ صف می‌تواند از نرخ مسیر بالاتر بزند | `engine/autopilot.go:445-447`، `TestBackoffNotResetBySpike` |
| `U` بدون «pressedها + spare» | shrink → کمبود → restore هر دقیقه (flap) | `TestAutopilotNoShrinkRestoreFlap` |
| loss دانلود = retrans آخرین pong / بایت‌های تیک ۲s | حکم تابع لرزش pong بود (۳۰٪ هرگز، ۹٪ تخلیه) | `engine/loss.go:17-25`، `engine/health.go:141-149` |
| مخرج loss = بایت/mss | لینک قاب‌کوچک ۱٫۱ تا ۱۰× پراتلاف‌تر خوانده می‌شد | `engine/loss.go:26-29` |
| مقایسهٔ لینک با میانهٔ لینک‌های پرمشغله | لینک‌های واقعاً پراتلاف میان throttled‌ها پنهان می‌ماندند | `engine/loss.go:48-51` |
| تخلیهٔ بی‌سقف هر نامزد loss | مسیر پراتلاف همهٔ ۳۰۰ لینک را یک‌جا تخلیه کرد | `CHANGELOG.md:1005-1007` |
| بستن همهٔ کاربران لینک degraded پس از ۴۵s | ۶۰ تا ۹۰ اتصال فعال در هر لینک قطع می‌شد | `engine/health.go:72-73` |
| نگه داشتن کاربران روی لینک degraded تا ۵ دقیقه | p90 ۱٫۶–۲٫۷s در مقابل ۰٫۲–۰٫۴ پس از اتصال دوباره | `engine/health.go:67-71`، `CHANGELOG.md:842-843` |
| شمردن لینک‌های پاسخ‌دهنده در ۲–۶s یا پرمشغله به‌عنوان «کند» در آزمون مسیر | دو لینک پراتلاف شبانه هر دو قاعده را برای همیشه خاموش می‌کردند | `engine/stuck.go:39-43`، `CHANGELOG.md:895-898` |
| آزمون مسیر کند فقط با شمارش | فشردگی ۱۹۰→۶۰Mbit/s ۳۳ لینک را stuck تخلیه کرد | `engine/linkmanager.go:1917-1922`، `CHANGELOG.md:1018-1027` |
| برش‌های اولیهٔ قاعدهٔ stuck | ۲۱۰، ۵۵، سپس ۲۷ لینک در فشردگی تخلیه شد | `CHANGELOG.md:967-968` |
| پایان کانال کنترل با یک timeout نوشتن | قاعدهٔ loss عملاً کور می‌شد | `CHANGELOG.md:950-952` |
| پینگ‌های کنترل لرزان روی لینک‌های پربار | هشدار نادرست loss دانلود را برگرداند | `CHANGELOG.md:768-770`، `engine/control.go:124-130` |
| بدون فیلتر `blockedMin` | نویسنده‌ای که همیشه داده دارد ~۹۰٪ «مسدود» خوانده می‌شد حتی روی مسیر نامحدود | `engine/health.go:49-52` |
| سقف refill که با زمان دو برابر شود | لینک‌های اول دوباره پر شدند (۱۹۲ در مقابل ۹۶ با سقف ثابت) | `engine/refill.go:30-34` |
| wedge guard بدون آستانهٔ `starveCalls` | خوانندهٔ آهسته توقف را تا ۲۳s+ پنهان می‌کرد (بیش از ۲۰s طرف مقابل) | `engine/wedge_test.go:211-214` |
| guard فقط با بافر پر (بدون فشار حافظه) | ۲۰ دانلود متوقف روی سرور ۲GB همهٔ سوکت‌ها را فشرد؛ ۱۱ لینک سالم تخلیه و ۴۴ کاربر قطع | `engine/mempressure.go:5-13` |
| قضاوت loss حین/بلافاصله پس از فشردگی | ۴–۸ لینک برای بازارسال‌های بازیابی تخلیه شد | `engine/linkmanager.go:1819-1823` |

---

## ۱۳. محدودیت‌های شناخته‌شده و مشاهده‌ها

### ۱۳.۱ محدودیت‌های مستند (در کد/مستندات)
- **پروب معکوس سقف خروجی را نمی‌داند** (`CHANGELOG.md:1096-1097`): autopilot ورودی `peerMax` ندارد؛ `capNote` فقط نمایشی است (`engine/linkmanager.go:609-621`). پروبی بالاتر از `max_links` خروجی هرگز مسلح نمی‌شود ⇒ abort با عقب‌نشینی ۶۰s→۸m.
- **هجوم ناگهانی روی پول بالا** (نه شروع/قطعی): refill hold پوشش نمی‌دهد؛ لینک‌های اول شلوغ می‌مانند (~۲۳۵ اتصال روی ۸ لینک) (`CHANGELOG.md:1086-1090`).
- **انفجار اتصال‌های تازه (>~۴۰/دقیقه/لینک) فشار واقعی را از جاگذاری پنهان می‌کند** ⇒ ۶–۱۰٪ اتصال‌ها روی لینک throttled (`CHANGELOG.md:1094-1096`).
- **پرسش باز V5**: افت اتصال پس از ازدحام با صف کم‌عمق (`VALIDATION.md:111-…`، `CHANGELOG.md:932-937`).
- guard گیرِ سمت خروجی را از لبه نمی‌بیند؛ تا ۴ جریان در انتظار dial کند پنل می‌توانند لینک را تا ۵s نگه دارند (`engine/stuck.go:55-60`، `README.md:242-245`).
- غیرلینوکس: بدون TCP_INFO ⇒ بدون loss و بدون فشار آپلود (`engine/health_other.go:7-9`).

### ۱۳.۲ مشاهده‌ها (برداشت خودم؛ بدون پیشنهاد تغییر کد)
- **مشاهده ۱ — autopilot نسبت به تأخیر کور است:** هیچ ورودی RTT/صف/loss به `decide` نمی‌رسد؛ فقط throughput، `pressed`، `flowing`. معیار نگه‌داشتن لینک فقط «افزایش throughput» است. `tcpStat.notsent`، `deliveryRate`، `sndbufUs` جمع‌آوری (و دو تای آخر روی سیم فرستاده) می‌شوند ولی هیچ تصمیمی از آن‌ها استفاده نمی‌کند (`engine/health_linux.go:32-42`، `engine/stats.go:236`)؛ `managedLink.goodput` و `managedLink.lossFrac` هم نوشته و هرگز خوانده نمی‌شوند (`engine/linkmanager.go:1791-1795`, `1852`).
- **مشاهده ۲ — در l3mtcp ترافیک TUN برای کف نامرئی است:** جریان L3 با `OpenRawStream` باز می‌شود (`engine/stream_iran.go:419-438`) و در `flows`/`Active()` نیست؛ پس `flowing`، کف (`fGrow/fHold`)، `U = fl60+2`، و شرط بستن retiring (`users==0 && Active()==0`, `engine/linkmanager.go:983`) آن را نمی‌بینند. ولی بایت‌هایش در `rate`، `G` و `wrBlocked` (فشار آپلود؛ و در خروجی فشار دانلود) هست. لینک TUN با هش rendezvous روی **همهٔ** لینک‌های زندهٔ L3 انتخاب می‌شود (`engine/l3_link.go:308-323`)، مستقل از serving/retiring/pressed؛ پس رشد/کوچک‌سازی/بستن لینک، بخشی از جریان‌های TUN را جابه‌جا می‌کند. اگر بار اصلی کاربر از hs0 عبور کند، رشد فقط از مسیر پروب (فشار) ممکن است و `U` (که فقط `fl60+2` و `p60+spare` است) می‌تواند سقف پایینی بگذارد. (اثر عملی در تولید: نامطمئن؛ بستگی دارد چه سهمی از ترافیک از TUN می‌گذرد.)
- **مشاهده ۳ — رشد به «همهٔ serving بالا» شرط شده:** `S ≥ T` (`engine/autopilot.go:548`). هر کسری serving (dial شکست، suspect، degraded در حال جایگزینی) پروب را تا پر شدن دوباره متوقف می‌کند.
- **مشاهده ۴ — تأخیر ذاتی رشد:** فشار ۲ از ۳ (≥۴s، دانلود یک تیک دیرتر) + `isShort` ۳ از ۵ + حداقل ۱۰ تیک تاریخچه + مسلح‌شدن + ۲ تیک نشست + حداقل ۵ تیک ارزیابی ⇒ سریع‌ترین موفقیت ~۱۶s پس از شروع پروب و پروب بعدی ≥۴s بعد. تخمین تقریبی من برای ۸→۳۰۰ فقط با پروب‌های پیاپی موفق: ~۱۰ پروب × ~۲۰s ≈ ۳ تا ۴ دقیقه (نامطمئن؛ اندازه‌گیری نشده). کف اما پرش مستقیم است (رسیدن به ۳۰۰ لینک در ~۵۷s در آزمون بار Q6 با ۲۴۰۰ کاربر فعال، `CHANGELOG.md:810` — به احتمال زیاد از مسیر کف؛ نامطمئن).
- **مشاهده ۵ — `belowSince` در طول پروب یخ می‌زند:** فقط در مرحلهٔ ۵ به‌روز می‌شود (`engine/autopilot.go:582-588`) و مراحل ۱ تا ۴ زودتر return می‌کنند. همچنین حکم «بی‌نتیجه» `lastGrowAt` را تازه نمی‌کند (`engine/autopilot.go:771-777`). پس پس از یک پروب بی‌نتیجه، اگر `!shortIn60` باشد، کوچک‌سازی می‌تواند زودتر از ۶۰s اسمی برسد. (با نیت مستند «spares را قاعدهٔ shrink برمی‌گرداند» سازگار است؛ اثر عملی نامطمئن.)
- **مشاهده ۶ — آستانهٔ حکم محافظه‌کار است:** آزمون فقط «قبول نادرست <۳٪» را تضمین می‌کند (`TestProbeVerdictNoiseFalsePass`)؛ نرخ «رد نادرست» پروب مفید روی مسیر پرنویز مستقیماً آزموده نشده (فقط غیرمستقیم در `TestSimTracksUpAndDown` ≥۸۲٪). هر رد نادرست تا ۸ دقیقه رشد را عقب می‌اندازد.
- **مشاهده ۷ — abort لرزش ندارد** (`engine/autopilot.go:634`) ولی شکست ±۲۰٪ دارد.
- **مشاهده ۸ — `needBW` به میانهٔ ظرفیت لینک‌ها تکیه دارد:** با ظرفیت ناهمگن لینک‌ها (برخی throttled، برخی آزاد) میانه می‌تواند کم‌یا‌بیش برآورد کند؛ و تا ≥۶ لینک متمایز در ۳۰ دقیقه pressed نشده باشند `needBW = 0` و کوچک‌سازی فقط با `needSat/fHold/hold` محدود می‌شود.
- **مشاهده ۹ — فاصلهٔ T و لینک فیزیکی:** کاهش T فقط retiring می‌کند؛ بستن منتظر صفر شدن کاربران است (یا ۳۱۰s بیکاری، یا ۲۰ دقیقه برای اتصال‌های غیر flowing). پس هزینهٔ حافظه/پنهان‌کاری لینک‌های اضافی تا چند دقیقه می‌ماند.
- **مشاهده ۱۰ — `pressed` به `TCP_NOTSENT_LOWAT` وابسته است:** معنای «۵۰٪ زمان مسدود» بر این فرض است که sendmsg پس از ~۳۲KiB صف منتظر می‌ماند (`engine/health.go:107-113`). مستندات Q6 می‌گوید سوکت پذیرفته‌شدهٔ MPTCP این گزینه را نادیده می‌گیرد و به همین دلیل MPTCP خاموش شد (`CHANGELOG.md:827-832`, `859-861`).
- **مشاهده ۱۱ — autopilot مشترک dgPool:** `dgPool` همین `decide` را با تعریف دیگری از `pressed` (افت صف حامل) صدا می‌زند (`engine/dgpool.go:1198`, `1390`)؛ هر تغییر در `autopilot.go` رفتار dgtun را هم عوض می‌کند و آزمون‌های dg باید هم اجرا شوند.
- **مشاهده ۱۲ — `fl5` کمینه و `fl60` بیشینه است:** رشد کف کند و محافظه‌کار (باید ۱۰ ثانیه پایدار باشد)، نگه‌داشتن سخاوتمندانه (قلهٔ ۶۰ ثانیه) — هیسترزیس عمدی.

---

## ۱۴. ارجاع به زیرسیستم‌های دیگر

| از | به | چه | path |
|---|---|---|---|
| `LinkManager.decideTarget` | `autopilot.decide` | هر تیک | `engine/linkmanager.go:600` |
| `dgPool` | `autopilot.decide` | هر تیک dgtun | `engine/dgpool.go:1390` |
| `LinkManager.sampleHealth` | `flowStats` (mtcpLink) | `flowing/open/recent` | `engine/linkmanager.go:1645-1649`، `engine/mtcp_link.go:97-130` |
| `sampleHealth` | `linkTCPStatsOf` → `tcpStats` | TCP_INFO | `engine/linkmanager.go:1652`، `engine/health_linux.go:17` |
| `sampleHealth` | `consumeRecord` ← `openStats/runStats` ← `serveStats` (خروجی) | فشار دانلود + فشار حافظهٔ طرف مقابل | `engine/linkmanager.go:1801`، `engine/stats.go:106-249` |
| `sampleHealth` | `ctrlWaitOf`، `peerLoss`، `rttMicros` ← `openControl` | stuck و loss دانلود | `engine/linkmanager.go:1657-1658`، `engine/control.go:59-…` |
| `sampleHealth` | `sessGuard.parkedAt` | استثنای wedged در stuck | `engine/linkmanager.go:1659-1663` |
| `sampleHealth` | `lossVerdicts` | حکم loss | `engine/linkmanager.go:1949` |
| `newSession` | `meteredConn`، `watchConn`، `guards.add` | اندازه‌گیری و نگهبان هر نشست (لبه و خروجی) | `engine/stream.go:172-202` |
| `relayStream` | `sessGuard.add/remove` | رله‌های کاربر تحت نظر | `engine/wedge.go:295-345` |
| `cmd/hs2/status.go` (`tcpMemWatch.check`) | `engine.SetTCPMemPressure` | فشار محلی | `cmd/hs2/status.go:854` |
| `serveStats` | `tcpMemPressure` | بیت فشار در رکورد | `engine/stats.go:238-240` |
| `queueDial`، `exitPool.runSlot`، `dgPool` | `linkGate.acquireIf/acquire` | آهنگ dial | `engine/linkmanager.go:911`، `engine/exit_pool.go:342`، `engine/dgpool.go:1377`, `1511` |
| `pickWait` (TCP کاربر) | `pickHeld` | refill hold | `engine/stream_iran.go:183-210`، `engine/refill.go:152-187` |
| `AddLink`/`queueDial` | `noteArrivalLocked` | شروع اپیزود refill | `engine/linkmanager.go:415`, `938` |
| `openPoolCtl` | `ctlTarget()` (T + جایگزین‌ها) | هدف برای خروجی معکوس | `engine/exit_pool.go:480-…`، `engine/linkmanager.go:1340` |
| `exitPool.setTarget` | — | clamp به `[min,max]` خروجی و شروع slot | `engine/exit_pool.go:161-185` |
| `publishStats`/`Stats` | `ap.cCap`، `ap.gPeak`، `ap.next`، `dec` | مانیتور زنده، `hs2 status`، فایل warm | `engine/linkmanager.go:2297-2372`، `cmd/hs2/status.go:300` |
| `cmd/hs2/main.go` (`linkEnvelope`، `warmLinks`، `drainIdle`) | `IranConfig{Min,Max,PerLink,DrainIdle,WarmLinks}` | پیکربندی | `cmd/hs2/main.go:476-484`، `engine/stream_iran.go:62-77` |

مرتبط در نقشه‌های دیگر: `04-linkmanager.md` (actuator، drain، reverse، pick)، `03-stream-core.md` (smux/shaper/relay)، `01-cmd-runtime.md` و `02-cmd-ops-tune.md` (پیکربندی، warm، tune، status).
