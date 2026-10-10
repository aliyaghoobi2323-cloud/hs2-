# LinkManager: پول لینک‌ها، انتخاب، تشخیص خرابی

> دامنه: `engine/linkmanager.go` (۲۳۸۲ خط، کامل خوانده شد) و همهٔ فایل‌هایی که منطق آن را کامل می‌کنند:
> `health.go`، `stuck.go`، `loss.go`، `refill.go`، `dialgate.go`، `burstlog.go`، `mtcp_link.go`، `l3_link.go`،
> `stream.go`، `stream_iran.go`، `stream_reverse.go`، `control.go`، `stats.go`، `peerinfo.go`، `wedge.go`، `mempressure.go`،
> بخش مربوط `exit_pool.go`، `tlscarrier/tune_linux.go`، `tlscarrier/carrier.go` و smux v1.5.24.
> همهٔ مسیرها نسبت به `/home/user/hs2-/hs2-src` هستند مگر خلافش گفته شود. ثبت پایه: `0812bc9`.
> هیچ فایلی در مخزن تغییر نکرد.

---

## ۰. خلاصهٔ فشرده (برای یادآوری سریع)

- `LinkManager` فقط در **حالت stream لبه (ایران)** ساخته می‌شود: `mtcp`، `l3mtcp`/`l3` و `tls` (تک‌لینک). دیتاگرام (`dgtun`) پول جدای خودش (`dgPool`) را دارد. سازنده‌ها: `engine/stream_iran.go:69` (معکوس) و `engine/stream_iran.go:72` (مستقیم).
- در `l3mtcp` **بار اصلی** (پورت‌های کاربر TCP/UDP) روی جریان‌های smux سوار است که `LinkManager.Pick` لینکشان را انتخاب می‌کند؛ TUN (`hs0`) فقط **کانال جانبی** است که با هش rendezvous و **بدون اطلاع از وضعیت LinkManager** روی لینک‌ها پخش می‌شود (`engine/l3_link.go:308-323`، `engine/stream_iran.go:419-438`؛ README: «not a bulk path»).
- یک تیک ۲ ثانیه‌ای (`healthTick`, `engine/health.go:18`) همه‌چیز را می‌راند: `reap → sampleHealth → heal → autoscale(reconcile) → drainTick → publishStats` (`engine/linkmanager.go:574-581`). در معکوس: `sampleHealth → reconcile → drainTick → sweepReverse → publishStats` (`engine/linkmanager.go:1132-1138`).
- اتصال کاربر **سنجاق** است: تا پایان عمرش روی همان لینک می‌ماند؛ مهاجرت جریان بین لینک‌ها وجود ندارد (`engine/linkmanager.go:23-27`).
- مسیرهای تشخیص خرابی (از سریع به کند): **wedge guard** (خوانندهٔ پارک‌شده، ≈۴–۶ ثانیه، فقط رله‌های گیر را می‌کشد) · **stuck** (پینگ کنترل ≥۶ ثانیه منتظر، ۲ نمونهٔ پیاپی ⇒ ≈۸–۱۵ ثانیه) · **suspect** (۱۲ ثانیه هیچ دریافت، فقط کنار گذاشتن) · **کانال جانبی L3** (۱۲ ثانیه سکوت نشست ⇒ `markDead`) · **TCP_USER_TIMEOUT=20s** (مرگ واقعی لینک) · **smux keepalive 24s** (۲۴–۴۸ ثانیه) · **loss** (≥۱۲٪ بازارسال، ۳ نمونه) ⇒ تخلیهٔ پله‌ای ۴۵/۹۰ ثانیه.
- چیزی که لینک را **می‌کشد**: مرگ نشست/سوکت (TCP، keepalive، خطای خواندن/نوشتن)، بستن لینک بازنشستهٔ خالی (`drainTick`)، پایان تخلیهٔ لینک degraded (کاربر صفر، ۹۰ ثانیه، یا پس‌دادن جا در سقف)، `closeAll`. چیزی که فقط **کنار می‌گذارد**: `suspect`، `retiring`، `degraded/draining`، `pressed` (فقط اولویت)، نبودن `infoDone`.

---

## ۱. نقش و جایگاه در کل سیستم

- **کارکرد:** نگه‌داشتن پولی از N لینک TLS موازی به یک مقصد (هر لینک یک جلسهٔ TLS واقعی با اثرانگشت Chrome و شکل‌دهی جداگانه)، پخش اتصال‌های تازهٔ کاربر بین لینک‌ها، سنجاق‌کردن هر اتصال به لینکش، اندازه‌گیری سلامت/فشار هر لینک، اجرای تصمیم autopilot دربارهٔ تعداد لینک «serving»، بازنشسته‌کردن و بستن مازاد، و جایگزینی لینک‌های بد به روش make-before-break (`engine/linkmanager.go:18-35`).
- **سود:** شکستن محدودیت سرعت به‌ازای‌هر‌اتصال؛ N لینک تا N برابر سقف هر اتصال حمل می‌کنند (`engine/linkmanager.go:25-27`).
- **دو حالت:**
  - **مستقیم** (لبه می‌شمارد و dial می‌کند): `dialer != nil`, `accept=false`؛ حلقهٔ `Run` (`engine/linkmanager.go:546-583`).
  - **معکوس** (لبه گوش می‌دهد، خروجی dial می‌کند): `accept=true`؛ لینک‌ها با `AddLink` تزریق می‌شوند و تعداد خواسته‌شده از کانال pool-control به خروجی فرستاده می‌شود (`engine/linkmanager.go:1119-1140`، `engine/exit_pool.go:470-563`).
- **سمت خروجی (خارج)** هیچ `LinkManager` ندارد؛ در مستقیم فقط می‌پذیرد (`engine/stream_kharej.go` → `RunKharej`) و در معکوس `exitPool` با «slot»ها کار می‌کند (`engine/exit_pool.go`).
- **مغز اندازه‌گیری** جداست: `autopilot` (`engine/autopilot.go`) بی‌قفل و بی‌ساعت؛ `LinkManager` «actuator» آن است (`engine/autopilot.go:11-15`). اینجا فقط رابط آن توضیح داده می‌شود (`apSample`/`apLink`/`apDecision`، `engine/autopilot.go:194-223`).

---

## ۲. اجزای اصلی

### ۲.۱ رابط‌ها

| نوع | محل | معنی |
|---|---|---|
| `LinkDialer` | `engine/linkmanager.go:40-42` | `DialLink(ctx) (Link, error)`؛ پیاده‌سازی: `mtcpDialer` (`engine/mtcp_link.go:246-292`) |
| `Link` | `engine/linkmanager.go:46-55` | `OpenStream`، `Active`، `Alive`، `Close` |
| `stream` | `engine/linkmanager.go:57-61` | Read/Write/Close |
| `rawStreamOpener` | `engine/linkmanager.go:214-216` | جریان بدون شمارش بار کاربر (کنترل، آمار، info، L3، pool) |
| `slowReclaimer` | `engine/linkmanager.go:220-222` | فهرست جریان‌های «جاری‌نبودن» (برای `retireForce`) |
| `idleReclaimer` | `engine/linkmanager.go:225-227` | فهرست جریان‌های بی‌کار |
| `metered` | `engine/health.go:217-220` | `meter()` و `tcpStats()`؛ لینک بدون آن هرگز soft-degrade یا pressed نمی‌شود |
| `flowSource` | `engine/mtcp_link.go:86-88` | آمار به‌ازای‌هر‌جریان (`flowStats`) |

### ۲.۲ پیاده‌سازی لینک: `mtcpLink` (`engine/mtcp_link.go:22-33`)

- یک `tlscarrier.Carrier` + یک `smux.Session` (لبه همیشه کلاینت smux است، چه مستقیم چه معکوس: `newEdgeLink`, `engine/mtcp_link.go:299-307`).
- `Alive()` = نشست بسته نشده و `dead` نخورده (`engine/mtcp_link.go:190-195`). **توجه:** فیلد `mtcpLink.dead` هیچ‌جا `true` نمی‌شود (فقط خوانده می‌شود، `engine/mtcp_link.go:44,191`)؛ یعنی برای mtcp مسیر «markDead» وجود ندارد و مرگ فقط از بسته‌شدن نشست می‌آید.
- `OpenStream` یک `countedStream` می‌سازد و `active` را بالا می‌برد (`engine/mtcp_link.go:59-73`)؛ `Active()` فقط جریان‌های کاربر را می‌شمارد. جریان‌های خام (`OpenRawStream`, `engine/mtcp_link.go:183`) شمرده نمی‌شوند.
- `downReason()` (`engine/mtcp_link.go:38-48`): دلیل از `watchConn` (اولین خطای خواندن/نوشتن)، وگرنه `"marked dead"` یا `"session ended (keepalive timeout or closed by the other server)"`.
- پیکربندی smux (`engine/mtcp_link.go:267-284`): `Version=2`، `KeepAliveInterval` تصادفی ۴–۸ ثانیه به‌ازای‌هر‌نشست، `KeepAliveTimeout=24s`، `MaxFrameSize=16KiB`، `MaxReceiveBuffer=8MiB`، `MaxStreamBuffer=2MiB`.
- لایه‌های اتصال (`engine/stream.go:172-203`): `raw TLS conn → shapedConn (شکل‌دهی طول) → meteredConn (شمارش بایت/انسداد) → watchConn (بستن نشست با اولین خطا + شمارش Read برای wedge) → smux`.

### ۲.۳ ساختار `LinkManager` (`engine/linkmanager.go:110-210`)

مهم‌ترین فیلدها:

| فیلد | محل | نقش |
|---|---|---|
| `dialer`, `accept` | 111-112 | مستقیم/معکوس |
| `min`, `max`, `perLink` | 113-115 | پاکت؛ `perLink` = جریان‌های **فعال** که هر لینک برایشان اندازه می‌شود |
| `mu sync.RWMutex`, `links []*managedLink`, `linkSeq` | 119-121 | فهرست پول؛ شناسه یکنوا |
| `users atomic.Int32` | 122 | کل اتصال‌های باز کاربر |
| `OnLink func(Link)` | 127 | برای هر لینک تازه در goroutine خودش (کنترل، آمار، info، pool، L3) |
| `gateInfo` | 132 | لینک تا پایان تبادل `kindInfo` کاربر نمی‌گیرد |
| `exitInfo` | 135 | آخرین پاسخ info خروجی برای لینک‌هایی که پاسخ خودشان دیر است |
| `ap`, `sample`, `dec`, `lastSampleAt` | 141-144 | مغز و ورودی/خروجی آن |
| `target atomic.Int32`, `pin` | 145-146 | تعداد serving خواسته‌شده؛ `pin` فقط آزمون |
| `targetDropAt` | 150 | زمان آخرین کاهش هدف (نگهبان بستن معکوس) |
| `targetCh` | 154 | برای بیدارکردن حلقه‌های pool-control |
| `promptFloor`, `stuckSlowAt`, `stuckSlowFor` | 164-166 | حالت «مسیر کند» و پایهٔ RTT برای قاعدهٔ stuck |
| `ctlDrain` | 168 | معکوس: تعداد draining که جایگزینشان از خروجی خواسته شده |
| `drainIdle` | 172 | حد بی‌کاری اتصال روی لینک بازنشسته |
| `retireCloses`, `reArrivals`, `noCloseUntil`, `growable` | 175-178 | نگهبان‌های churn معکوس |
| `shortLived` | 179 | شمارندهٔ «مرگ زودهنگام» |
| `gate`, `dialing`, `dialEpoch`, `dialedN`, `dialFails`, `dialErr`, `dialWG`, `failStreak` | 189-200 | صف و دروازهٔ dial |
| `warm` | 203 | اندازهٔ شروع گرم |
| `hold refillHold` | 207 | اپیزود refill |
| `upLog`, `downLog`, `closeLog`, `replLog` | 209 | لاگ‌های تاشونده (`burstLog`) |

### ۲.۴ ساختار `managedLink` (`engine/linkmanager.go:229-296`)

| گروه | فیلدها | محل | معنی |
|---|---|---|---|
| هویت | `link`, `id`, `born`, `dead` | 230-233 | `dead` **استفاده نمی‌شود** |
| بار | `users` (اتمیک، داخل `Pick` زیر قفل بالا می‌رود) | 237 | اتصال‌های جای‌داده‌شده |
| شمارنده‌های قبلی | `prevRd`, `prevWr`, `prevRetrans`, `prevBlocked`, `prevBusy`, `prevRwnd`, `prevSegsOut`, `prevPeer` | 242-254 | برای دلتاهای هر تیک |
| تشخیصی | `goodput` (EWMA، فقط محاسبه می‌شود و هیچ‌جا خوانده نمی‌شود)، `lossFrac` (فقط نوشته می‌شود) | 248, 255 | |
| loss | `upStreak`, `dnStreak`, `lowStreak`, `upBadAt`, `dnBadAt` | 249-252 | رگه‌های بد هر جهت |
| stuck | `stuckStreak`, `stuck` | 256-257 | |
| سلامت | `sampled`, `degraded`, `draining`, `drainSince`, `drainNoted`, `drainReplace`, `drainReclaim` | 258-264 | |
| اندازه | `retiring`, `retireSince`, `servingSince`, `upHist`, `dnHist`, `pressed` | 267-272 | |
| زنده‌بودن | `suspect`, `lastRx`, `rxSeen` | 273, 292-293 | |
| نرخ | `rates[5]`, `doms[3]`, `nSamples`, `rate`, `rate10`, `sustained` | 274-279 | |
| جریان‌ها | `flowing`, `open`, `recent`, `lastByte` | 280-283 | از `flowStats` |
| انتخاب | `picks`, `pickHist[2]` | 284-285 | پنجرهٔ ۳ نمونه‌ای جای‌گذاری |
| آمار خروجی | `lastRec`, `haveRec`, `lastRecAt` | 286-288 | |
| معکوس | `poolRefused`, `ctlLive`, `bornSpare` | 289-291 | |
| بازپس‌گیری | `reclaiming`, `heldLogAt` | 294-295 | |

`serving()` = `!retiring && !degraded && !draining && Alive() && !suspect` (`engine/linkmanager.go:299-301`).

### ۲.۵ توابع کلیدی

| تابع | محل | کار |
|---|---|---|
| `NewLinkManager` | 324-347 | پیش‌فرض: `min≥1`، `max≥min`، `perLink=8`؛ `target=warmSize`؛ `drainIdle=310s`؛ ۴ `burstLog` |
| `SetWarm`, `warmCount`, `warmSize` | 354-370, 742-751 | اندازهٔ شروع |
| `SetDrainIdle` | 374-379 | ≤۰ ⇒ هرگز |
| `jitterGap` / `closeJitter` | 394-401 | ۴۰–۱۶۰ms / ۵۰–۲۵۰ms |
| `AddLink`, `noteSurplusArrivalLocked`, `DropLink` | 409-496 | معکوس |
| `aliveLocked`, `countsLocked` | 511-533 | شمارش؛ serving بدون suspect/degraded/draining |
| `Run` | 546-583 | حلقهٔ مستقیم |
| `decideTarget`, `setTarget`, `notifyCtl`, `capNote` | 590-665 | مرز با autopilot |
| `reconcile` | 767-862 | حرکت پول به T |
| `wantsDial`, `queueDial` | 869-950 | dial خارج از تیک |
| `drainTick`, `closesPerTick`, `reclaimIdle` | 959-1117 | بستن بازنشسته‌ها |
| `runAccept`, `sweepReverse` | 1124-1210 | معکوس |
| `drainStepLocked` | 1216-1239 | پله‌های تخلیهٔ degraded |
| `drainHeadroom`, `slotsBackLocked`, `exitCeilingLocked`, `replacingLocked`, `ctlTarget` | 1253-1349 | جا و سقف هنگام تخلیه |
| `reclaimStalled` | 1361-1398 | بستن اتصال‌های بی‌حرکت روی لینک degraded |
| `reap`, `shortLivedLinks.note` | 1408-1462 | حذف مرده‌ها |
| `pickKey`, `newPickKey`, `Pick`, `pickLocked`, `placeLocked`, `releaseFor` | 1470-1586 | انتخاب لینک |
| `sampleHealth` | 1619-2024 | قلب اندازه‌گیری و حکم‌ها |
| `consumeRecord` | 2032-2064 | فشار دانلود از رکورد خروجی |
| `heal` | 2089-2182 | make-before-break |
| `dialRoom(Locked)` | 2193-2208 | ظرفیت dial |
| `Stats`, `publishStats`, `PoolStats` | 2216-2372 | پایش |
| `closeAll` | 2374-2382 | خاموشی |

### ۲.۶ goroutineها

| goroutine | از کجا | دوره/عمر |
|---|---|---|
| `lm.Run` / `runAccept` | `engine/stream_iran.go:133` | تیک ۲s تا پایان ctx |
| هر `queueDial` | `engine/linkmanager.go:908-949` | یک dial؛ پشت `linkGate` |
| `OnLink` ← `openControl`، `openStats`، حلقهٔ `openInfo`، (معکوس) `openPoolCtl`، (TUN) `openL3`→`serveLink` | `engine/stream_iran.go:101-129` | عمر لینک |
| `runRefill` | `engine/refill.go:116-120` | ≤۱۰s، تیک ۱۰۰ms |
| `reclaimIdle` (هر لینک بازنشسته حداکثر یکی) | `engine/linkmanager.go:1037` | یک گذر |
| `reclaimStalled` (هر لینک draining حداکثر یکی) | `engine/linkmanager.go:1363` | یک گذر؛ هر بستن در goroutine خودش |
| بستن با جیتر | `engine/linkmanager.go:1031-1034` | `sleep(closeJitter)` سپس `Close` |
| `guards.run` (سراسری، wedge) | `engine/wedge.go:211,220-226` | تیک ۲s |
| `watchSession` کانال L3 | `engine/l3_link.go:127-128,143-161` | تیک ۲s |
| `writeLoop` L3 | `engine/l3_link.go:202-243` | keepalive ۲s یا ~۱۰s |
| smux `keepalive` | smux `session.go:399-422` | NOP هر ۴–۸s، بررسی هر ۲۴s |
| `burstLog.flush` | `engine/burstlog.go` (AfterFunc) | پایان پنجرهٔ ۱۰s |

---

## ۳. جریان داده و کنترل، گام‌به‌گام

### ۳.۱ راه‌اندازی لبه (`RunIran`, `engine/stream_iran.go:56-176`)

1. ساخت `LinkManager` (مستقیم با `cfg.Dialer`، معکوس با `nil` و `accept=true`).
2. `SetDrainIdle(cfg.DrainIdle)` فقط اگر ≠۰ (`engine/stream_iran.go:74-76`)؛ `SetWarm(cfg.WarmLinks)` (77)؛ `gateInfo=true` (80).
3. TUN: `l3Set` ساخته و `pumpTun` و `logDrops` اجرا می‌شوند (90-95).
4. `OnLink` برای هر لینک تازه: `openControl`، `openStats`، حلقهٔ `openInfo` (تا ۳ بار، سپس هر ۱ دقیقه)، معکوس: `openPoolCtl`، TUN: `openL3` (101-129).
5. `go lm.Run(ctx)`؛ معکوس: `acceptReverseLinks` (133-136).
6. پورت‌های کاربر (`ListenReuse`) با backoff روی خطای accept (138-173).

**شروع گرم** (`engine/linkmanager.go:554-566`): پول در `warmCount()` بالا می‌آید (پیش‌فرض `warmStartLinks=8` بریده‌شده در پاکت، یا هدف قبل از ری‌استارت). در لحظهٔ شروع `min(warm, ceil(warm/4)+gateInflight)` dial صف می‌شوند؛ بقیه را `reconcile` تیک‌به‌تیک اضافه می‌کند. فایل warm: `cmd/hs2/status.go:192-245` (کهنه‌تر از ۱۵ دقیقه نادیده؛ فقط بعد از ۱ دقیقه کارکرد نوشته می‌شود؛ `warmLinks` فقط اندازه را **بالا** می‌برد: `cmd/hs2/main.go:280-293`).

### ۳.۲ تیک مستقیم (`Run`, `engine/linkmanager.go:567-581`)

ترتیب دقیق هر ۲ ثانیه:
1. `reap()` — مرده‌ها را بیرون می‌کشد، `Close`، لاگ `link N down: <reason>`، شمارش short-lived (1408-1434).
2. `sampleHealth()` — اندازه‌گیری، suspect، فشار، loss، stuck، حکم‌ها، ساخت `apSample` (1619-2024).
3. `heal(ctx)` — degraded جدید ⇒ draining + جایگزین؛ پله‌های تخلیه؛ پس‌دادن جا (2089-2182).
4. `autoscale(ctx)` = `reconcile(ctx, decideTarget())` (755-757).
5. `drainTick()` — بستن بازنشسته‌های خالی، بازپس‌گیری بی‌کارها (959-1056).
6. `publishStats()` (2297-2309).

### ۳.۳ تیک معکوس (`runAccept`, `engine/linkmanager.go:1124-1140`)

`sampleHealth → reconcile(decideTarget) → drainTick → sweepReverse → publishStats`. `reap` و `heal` صدا زده نمی‌شوند؛ کار آن‌ها را `sweepReverse` (مرده‌ها و degradedها) و `DropLink` (به‌محض بسته‌شدن نشست، از `acceptReverseLinks`) انجام می‌دهند. `reconcile` در معکوس هرگز dial نمی‌کند (`engine/linkmanager.go:826-828`).

### ۳.۴ اتصال تازهٔ کاربر TCP (مسیر بار اصلی در l3mtcp)

1. `serveUserTCP` (`engine/stream_iran.go:260-268`) → `openStream(ctx, lm, udp=false, port)`.
2. `openStream` (241-258): تا ۳ تلاش؛ هر بار `pickWait(ctx, lm, hold=true)`، سپس `link.OpenStream()` و نوشتن سرآیند (`kindTCP` یا `kindTCPPort+port`؛ `userStreamHeader`, 219-237). شکست ⇒ `release()` و تلاش بعدی.
3. `pickWait` (183-210): تا ۴۰ بار × ۱۵۰ms (≈۶ ثانیه). در حالت hold از `pickHeld` و در غیر آن `Pick`. اگر لینکی نیست ⇒ پس از ≈۶s اتصال کاربر بسته می‌شود.
4. `pickHeld` (`engine/refill.go:152-187`): بیرون از اپیزود = `Pick`. در اپیزود: اول صف قبلی را جا می‌دهد (`admitLocked`)، سپس لینکی با `users < cap` (cap = سهم منصفانه)، وگرنه در صف منتظر (حداکثر ۱۰s).
5. `Pick` (`engine/linkmanager.go:1514-1523`): زیر قفل **نوشتن** `m.mu`، `pickLocked(0)` و `placeLocked` (users++، picks++، m.users++) و تابع release یک‌باره (`sync.Once`).
6. `relayStream(user, st, guardOf(st))` (`engine/wedge.go:295-345`): دو کپی؛ نوشتن به اپ زیر نظر wedge guard؛ با مرگ جریان (`GetDieCh`) پس از `relayDieGrace=5s` هر دو سر بسته می‌شود.

**`pickLocked(limit)`** (`engine/linkmanager.go:1548-1586`) — سه لایه:
- لایه ۰: نه retiring، نه degraded، نه draining، نه suspect (یعنی serving).
- لایه ۱: retiring هم مجاز (ولی نه degraded/draining/suspect).
- لایه ۲: هر لینک زنده (degraded/draining/suspect هم).
- در همهٔ لایه‌ها: لینک مرده رد؛ اگر `gateInfo` و لینک meter دارد و `infoDone` نشده ⇒ رد؛ اگر `limit>0` و `users≥limit` ⇒ رد (کلاهک hold).
- مرتب‌سازی با `pickKey{pressed, load, users}` (1470-1484): اول نافشرده؛ سپس کمترین `load = flowing + picks`؛ سپس کمترین `users`؛ تساوی کامل ⇒ تصادفی (reservoir، 1571-1579).
- **کلاهک انفجار** (`newPickKey`, 1492-1494): لینکی که در ۳ نمونهٔ اخیر (`pickWindow=3`، ≈۴–۶ ثانیه) `≥ max(1, perLink/2)` اتصال تازه گرفته (با per_link=8 یعنی ۴) «فشرده» حساب می‌شود تا اندازه‌گیری فشار عقب‌مانده جبران شود.

### ۳.۵ UDP کاربر

`serveUserUDP` (`engine/stream_iran.go:304-416`): هر آدرس کلاینت یک `udpFlow` با صف ۲۵۶ دیتاگرام/۵۱۲KB و نویسندهٔ خودش؛ جریانش با `openStream(..., udp=true)` (یعنی **بدون hold**) باز می‌شود. خرابی جریان ⇒ `gone()`؛ دیتاگرام بعدی جریان تازه (و لینک تازه) می‌سازد. بی‌کاری ۲ دقیقه (`udpIdle`, `engine/stream.go:270`) ⇒ پایان.

### ۳.۶ کانال جانبی TUN در l3mtcp

- روی هر لینک تازه `openL3` یک جریان خام `kindL3` باز می‌کند و `newStreamL3Link` می‌سازد (`engine/stream_iran.go:419-438`).
- انتخاب لینک برای هر بسته: `l3Set.pick` با hash پنج‌تایی (FNV-1a) و rendezvous روی همهٔ `l3Link`های زنده (`engine/l3_link.go:308-323`). **هیچ ارتباطی با `pickLocked`، `degraded`، `suspect`، `retiring` یا `draining` ندارد.** سمت خروجی هم مستقل از دید لبه همین کار را می‌کند (`engine/stream_kharej.go:245-253`).
- صف هر لینک ۲۵۶ بسته، سقف ماندگاری ۶۰ms، هرگز مسدود نمی‌کند (پر ⇒ دور ریخته) (`engine/l3_link.go:53,62,191-199,332-357`).
- مرگ کانال جانبی (`markDead`, 183-189): خطای نوشتن (مهلت ۵s در `streamPkt.WriteRaw`, `engine/stream.go:214-218`)، مهلت خواندن ۳۰s (`l3StreamDeadAfter`)، یا **۱۲ ثانیه سکوت کل نشست** (`l3SessionSilent`, `engine/l3_link.go:87,143-161`، بررسی هر ۲s). پس از آن جریان‌هایش با rendezvous به لینک‌های دیگر می‌روند (فقط جریان‌های همان لینک جابه‌جا می‌شوند).
- keepalive کانال: ۲s، یا ~۱۰s±۲۰٪ اگر طرف دیگر `capL3Quiet` اعلام کرده باشد (`engine/l3_link.go:67,79,130-136`).
- چون جریان L3 در `Active()` شمرده نمی‌شود، لینک بازنشسته را نگه نمی‌دارد؛ بستن لینک بازنشسته جریان‌های TUN آن را جابه‌جا می‌کند.

### ۳.۷ `sampleHealth` گام‌به‌گام (`engine/linkmanager.go:1619-2024`)

1. `dt` = فاصلهٔ واقعی از نمونهٔ قبل (حداقل ۱ms)؛ `recentWin=drainIdle` (یا پیش‌فرض) (1620-1633).
2. **بیرون از قفل:** کپی فهرست؛ برای هر لینک زنده: `flowStats` (فعال/باز/اخیر/آخرین بایت)، شمارنده‌های meter، `TCP_INFO`، `peerSeen`، `peerLoss`، رکورد آمار خروجی، `statsState`، `ctrlWait`، `ctrlAnsweredSent`، و `wedged` (خوانندهٔ نشست در ۶s اخیر پارک شده) (1635-1665).
3. فشار حافظهٔ TCP طرف مقابل از رکوردها (`statsFlagMemPressure`) و `press = memPressure()` (1669-1677).
4. **زیر قفل نوشتن** (1697-2001):
   - `calm` = از آخرین تیک کند به‌اندازهٔ `stuckRecoverFor()` گذشته و فشار حافظه نیست (1703).
   - لینکی که بعد از کپی آمده فقط شمرده می‌شود (1706-1711).
   - چرخش `pickHist` و صفر کردن `picks` (1715-1716)؛ کپی `flowing/open/recent/lastByte`.
   - شمارش `aged/poolOK` (لینک‌های ≥۴s برای تشخیص pool-control) و `nOK/nOld` (آمار خروجی).
   - **suspect** (1735-1744): اگر `rd` تغییر کرده ⇒ `lastRx=now`؛ `suspect = rxSeen && now-lastRx ≥ 12s`؛ در گذار به suspect لاگ.
   - نمونهٔ اول فقط پایه می‌گذارد (1745-1759).
   - دلتاها، نرخ (`rate`, `rate10` میانگین ۵ تیک، `sustained` کمینهٔ ۳ تیک جهت غالب)، `goodput` (1760-1795).
   - **فشار آپلود** (1797-1799): `perTick(dWr) ≥ 16KiB && blocked/dt ≥ 0.5 && upRwnd < 0.5`؛ ۲ از ۳.
   - **فشار دانلود** (1801-1805): `consumeRecord` از رکورد خروجی؛ ۲ از ۳؛ رکورد کهنه‌تر از ۶s حساب نمی‌شود.
   - `pressed = serving() && (up || dn)` (1806).
   - `pollStats` فقط اگر لینک ≥16KiB در تیک جابه‌جا کرده (1812-1814).
   - **loss** (1824-1860): فقط برای لینک‌های غیر degraded/draining؛ رگه‌های جدا برای آپلود (هر تیک) و دانلود (هر پنجرهٔ pong)؛ در `!calm` رگه صفر می‌شود؛ نمونهٔ ساکت رگه‌ای با آخرین بد ≤۲۰s را نگه می‌دارد؛ نامزد وقتی `streak ≥ 3 && bad`.
   - **stuck** (1864-1901): `waits = !degraded && !draining && !wedged && ctrlWait ≥ 6s && moved < 96KiB`؛ اگر `waits && !suspect` ⇒ `stuckStreak++` و در ≥۲ نامزد؛ وگرنه رگه صفر و اگر لینک شلوغ است و سریع پاسخ می‌دهد (RTT و انتظار < ۲s) شاهد «مسیر سالم» ثبت می‌شود.
5. **حکم مسیر کند** (1923-1943): میانهٔ RTT شاهدان؛ پایهٔ `promptFloor` (کمینهٔ میانه‌های دقیقه‌ای ۱۰ دقیقهٔ اخیر، وقتی ≥۳ شاهد)؛ `inflated` اگر میانه > max(0.5s, 4×پایه)؛ `slow = press || (waitingN ≥ 2 && (waitingN > len(answering) || inflated))`؛ دورهٔ کندی جمع می‌شود.
6. **حکم loss** اگر `!recovering` (`lossVerdicts`, `engine/loss.go:180-225`).
7. لاگ مسیر کند/فشار حافظه هر دقیقه حداکثر یک بار (1951-1967).
8. **حکم stuck** اگر شاهدی هست و `!recovering` (1976-1991): نامزد باید (الف) شاهدی پینگی **بعد از** قدیمی‌ترین پینگ منتظرش پاسخ گرفته باشد (`lastAns > c.sent`)، (ب) `perTick < max(12KiB, میانهٔ جابه‌جایی شاهدان/۲)`، (ج) جا (`drainHeadroom(max) - stuckNow`) باشد؛ طولانی‌ترین انتظارها اول ⇒ `degraded=stuck=true`.
9. `growable` (معکوس) و لاگ «بدون pool control» (1995-2018)؛ `exitStats`؛ راهنمای rwnd هر ۱۰ دقیقه.

### ۳.۸ `reconcile(T)` (`engine/linkmanager.go:767-862`)

1. دسته‌بندی: serving / retiring (لینک مرده، degraded، draining، suspect در هیچ‌کدام نیستند).
2. **رشد:** اگر serving<T و retiring هست ⇒ اول بازنشسته‌ها برمی‌گردند (بیشترین `open`، سپس تازه‌ترین `lastByte`)؛ `servingSince=now` (برای انتساب probe).
3. **کوچک‌شدن:** اگر serving>T ⇒ مازاد به retiring می‌رود؛ قربانی‌ها به ترتیب کمترین `flowing`، `recent`، `open`، `rate10` (زودتر خالی‌شونده‌ها)؛ `pressed=false`.
4. لاگ‌ها؛ در معکوس همین‌جا تمام.
5. لاگ dialهای موفق (`dialedN`) و هر ۳۰ ثانیه لاگ شکست dial.
6. **صف dial:** `n = T - S - inflight`، حداکثر `step=ceil(T/4)`، حداکثر `dialRoom - inflight`، حداکثر `step + gateInflight - inflight`.

### ۳.۹ `queueDial` و دروازه (`engine/linkmanager.go:895-950`, `engine/dialgate.go`)

- `dialing++` همزمان (پیش از goroutine) تا تیک بعد دوباره صف نکند.
- `valid()`: epoch عوض نشده و (جایگزین است یا `wantsDial()`)؛ `wantsDial` = جا هست و serving<target (869-882).
- `gate.acquireIf(ctx, valid)`: حداکثر ۸ همزمان، فاصلهٔ شروع ۴۰–۱۶۰ms (~۱۰ در ثانیه)؛ valid پیش از رزرو فاصله بررسی می‌شود؛ پس از خواب فاصله دوباره بررسی (`engine/linkmanager.go:916`).
- `DialLink` ← `tlscarrier.DialFrom` با connect timeout **۸s** و `authTimeout` ۱۰s (`tlscarrier/carrier.go:162,169`؛ `tlscarrier/auth.go:59`). **ctx به dial نمی‌رسد** (DialFrom ctx ندارد).
- شکست: `failStreak++`؛ اگر `≥3` یا هیچ لینک زنده‌ای نیست ⇒ `dialEpoch++` و همهٔ dialهای صف‌شده بی‌تلاش رها می‌شوند (یک peer مرده = تقریباً یک تلاش در هر تیک) (920-927).
- موفقیت: `failStreak=0`، افزودن به پول زیر قفل (+`noteArrivalLocked`)، لاگ (جایگزین با `replLog`)، `OnLink`.
- **مستقیم هیچ backoff نمایی یا «scout» ندارد**؛ بازتلاش با تیک ۲s است (بر خلاف `exitPool` در معکوس، `engine/exit_pool.go:41-56`).

### ۳.۱۰ `heal` (`engine/linkmanager.go:2089-2182`)، فقط مستقیم

1. هر `degraded && !draining` ⇒ `draining=true`، `drainReplace=!retiring`، `retiring=false`، `drainSince=now`؛ اگر serving بوده `need++`.
2. برای هر need: بازگرداندن بازنشستهٔ زندهٔ غیر degraded با بیشترین `open` (بدون بررسی suspect) ⇒ لاگ «back in service to replace a degraded link».
3. باقی need با dial جایگزین (`replacement=true`، می‌تواند از max بگذرد): حداکثر `drainHeadroom(max)` در تیک و حداکثر `max + headroom - len(links) - dialing`.
4. برای هر draining: `drainStepLocked` ⇒ حذف (کاربر صفر یا >۹۰s) یا بازپس‌گیری بی‌حرکت‌ها (>۴۵s یا stuck).
5. `slotsBackLocked` با room = `max + headroom - len - inflight`.
6. بستن، لاگ‌ها (حداکثر ۳ خط + شمارش)، `reclaimStalled`، «retired N drained link(s)».

### ۳.۱۱ `drainTick` و بازنشسته‌ها (`engine/linkmanager.go:959-1117`)

- نگهبان معکوس: `now - targetDropAt ≥ 4s` و دست‌کم یک لینک ≥۴s که pool-control را رد نکرده و بیرون از نگهداشت churn (963-972).
- لینک بازنشستهٔ زنده (نه draining) بسته می‌شود اگر: نگهبان، `len(closing) < closesPerTick(R)`، `users==0 && Active()==0`، (معکوس) سن ≥۴s، (bornSpare) سن ≥۳۰s (983-988). زیر قفل از پول بیرون می‌رود و بیرون از قفل با جیتر ۵۰–۲۵۰ms بسته می‌شود.
- `closesPerTick(R) = min(max(2, ceil(R/32)), 8)` (1062-1064).
- بازپس‌گیری بی‌کارها اگر `drainIdle>0 && open>0` (یک goroutine در هر لینک) ⇒ `reclaimIdle`: جریان‌های بی‌بایت به مدت `drainIdle` (حداکثر ۱۶ در گذر)؛ اگر بازنشستگی ≥۲۰ دقیقه (`retireForce`) جریان‌های «جاری‌نبودن» هم (FIN، نه RST)؛ جریانی که بین انتخاب و بستن تکان خورده بخشیده می‌شود (1082-1117).
- لاگ «held» در ۱۵ دقیقه و سپس هر ساعت (حداکثر ۴ نمونه + شمارش).

### ۳.۱۲ معکوس: ورود/خروج و churn

- `acceptReverseLinks` (`engine/stream_reverse.go:47-101`): سقف پذیرش `2×max+8` لینک **زنده**؛ رد ⇒ ۵s نگه‌داشتن سپس بستن (لاگ دقیقه‌ای)؛ `newEdgeLink` ⇒ `AddLink`؛ نگه‌داشتن تا بسته‌شدن نشست ⇒ `DropLink`.
- `AddLink` (409-438): اگر serving ≥ target ⇒ «born retiring/spare» (`bornSpare`)؛ این لینک ۳۰s (`bornSpareGrace`) بسته نمی‌شود چون ممکن است جایگزین لینکی باشد که مرده ولی لبه هنوز نفهمیده.
- نگهبان churn (`noteSurplusArrivalLocked`, 442-470): ۳ ورود مازاد هر کدام ≤۱۰s پس از یک retire-close، در پنجرهٔ ۳ دقیقه ⇒ ۱۰ دقیقه هیچ retire-close.
- `sweepReverse` (1145-1210): مرده‌ها بسته؛ degraded ⇒ draining (`drainReplace=!retiring`)؛ پله‌ها؛ `slotsBackLocked` با room = سقف خروجی - تعداد (اگر growable)؛ `ctlDrain` عوض شد ⇒ `notifyCtl` **پیش از** بستن‌ها.
- `ctlTarget = target + ctlDrain` (1340-1349) ⇒ جایگزین لینک degraded فوراً از خروجی خواسته می‌شود.
- `openPoolCtl` (`engine/exit_pool.go:480-560`): ارسال هدف در لحظه، با هر تغییر، و تازه‌سازی دوره‌ای ۳s روی دو لینک قدیمی‌تر زنده (`poolCtlFast`, `engine/linkmanager.go:708-737`) و ۳۰s روی بقیه؛ تغییر روی بقیه با تأخیر تصادفی ≤۱.۵s پخش می‌شود؛ EOF در حالی که لینک زنده است ⇒ `markPoolRefused`.

---

## ۴. جدول ثابت‌ها، آستانه‌ها، بافرها و زمان‌سنج‌ها

### ۴.۱ خود LinkManager

| نام | مقدار | محل | معنی |
|---|---|---|---|
| `pressBlocked` | 0.5 | `engine/linkmanager.go:70` | سهم زمان انتظار نویسنده برای شبکه تا «فشرده» |
| `pressMinBytes` | 16KiB/تیک | `engine/linkmanager.go:71` | حداقل جابه‌جایی برای قضاوت فشار؛ و شرط `pollStats` |
| `rwndShareMax` | 0.5 | `engine/linkmanager.go:72` | اگر سهم پنجرهٔ گیرنده بیشتر باشد فشار نیست |
| `statsStale` | 6s | `engine/linkmanager.go:75` | رکورد خروجی کهنه‌تر حساب نمی‌شود |
| `statsGap` | 7s | `engine/linkmanager.go:76` | فاصلهٔ بیشتر بین دو رکورد ⇒ پایه‌گذاری دوباره |
| `drainIdleDefault` | 310s | `engine/linkmanager.go:82` | بی‌کاری اتصال روی لینک بازنشسته (بالاتر از connIdle ۳۰۰s xray) |
| `idleReclaimMax` | 16 | `engine/linkmanager.go:83` | بستن بی‌کار در هر گذر هر لینک |
| `maxClosesPerTick` | 2 | `engine/linkmanager.go:84` | کف بستن بازنشستهٔ خالی در تیک |
| `retireForce` | 20m | `engine/linkmanager.go:91` | پس از آن جریان‌های «جاری‌نبودن» هم بسته می‌شوند |
| `heldLogFirst` / `heldLogEvery` | 15m / 1h | `engine/linkmanager.go:93-94` | لاگ لینک بازنشستهٔ نگه‌داشته |
| `churnReArrive` | 10s | `engine/linkmanager.go:101` | ورود دوباره پس از retire-close |
| `churnWindow` | 3m | `engine/linkmanager.go:102` | |
| `churnTrips` | 3 | `engine/linkmanager.go:103` | |
| `churnHold` | 10m | `engine/linkmanager.go:104` | توقف retire-close |
| `dialFailLogEvery` | 30s | `engine/linkmanager.go:106` | |
| `rwndHintEvery` | 10m | `engine/linkmanager.go:107` | |
| `bornSpareGrace` (var) | 30s | `engine/linkmanager.go:308` | لینک مازاد معکوس پیش از بستن |
| `suspectAfter` | 12s | `engine/linkmanager.go:317` | هیچ دریافت ⇒ suspect |
| `jitterGap` | 40–160ms | `engine/linkmanager.go:394-396` | فاصلهٔ شروع dial |
| `closeJitter` | 50–250ms | `engine/linkmanager.go:399-401` | فاصلهٔ بستن‌ها |
| `dialFailRun` | 3 | `engine/linkmanager.go:865` | شکست پیاپی تا رهاکردن صف |
| `drainHeadroom(n)` | `max(2, ceil(n/8))` | `engine/linkmanager.go:1253` | سهم تخلیهٔ همزمان و فرارفتن از max (max=32⇒4، 48⇒6، 64⇒8) |
| `drainCloseGap` | 20ms | `engine/linkmanager.go:1402` | فاصلهٔ بستن اتصال‌های بی‌حرکت |
| `shortLinkLife` / `shortLinkRun` | 20s / 3 | `engine/linkmanager.go:1446-1447` | راهنمای «مسیر TCP را می‌کشد» |
| `pickWindow` | 3 نمونه | `engine/linkmanager.go:1497` | پنجرهٔ کلاهک انفجار |
| کلاهک انفجار | `max(1, perLink/2)` | `engine/linkmanager.go:1493` | |
| گام dial هر تیک | `ceil(T/4)` | `engine/linkmanager.go:846` | |
| صف شروع | `min(warm, ceil(warm/4)+8)` | `engine/linkmanager.go:564` | |
| `closesPerTick` | `min(max(2,ceil(R/32)),8)` | `engine/linkmanager.go:1062-1064` | |
| تکرار info | ۳ بار، فاصلهٔ ۱۰s، سپس هر ۱m | `engine/peerinfo.go:85-89` | |

### ۴.۲ سلامت، loss، stuck

| نام | مقدار | محل | معنی |
|---|---|---|---|
| `healthTick` | 2s | `engine/health.go:18` | دورهٔ تیک پول |
| `gpAlpha` | 0.4 | `engine/health.go:21` | EWMA goodput (تشخیصی) |
| `activeBytes` | 96KiB | `engine/health.go:25` | حداقل جابه‌جایی برای قضاوت loss؛ «کم جابه‌جاکردن» در stuck |
| `mss` | 1400 | `engine/health.go:28` | تبدیل بایت به بسته (جایگزین) |
| `lossFrac` | 0.12 | `engine/health.go:31` | آستانهٔ بازارسال |
| `degradeStreak` | 3 | `engine/health.go:34` | نمونه‌های بد پیاپی |
| `flowTau` / `flowingRate` / `flowRecent` / `flowSteadyRate` | 10s / 2KiB/s / 6s / 256B/s | `engine/health.go:41-48` | تعریف جریان «فعال» |
| `blockedMin` | 1ms | `engine/health.go:53` | نوشتن کوتاه‌تر انتظار شبکه نیست |
| `maxDrain` | 45s | `engine/health.go:74` | پس از آن اتصال‌های بی‌حرکت بسته |
| `drainStall` | 15s | `engine/health.go:75` | تعریف «بی‌حرکت» |
| `maxDrainActive` | 90s | `engine/health.go:76` | سقف عمر لینک degraded |
| `warmStartLinks` | 8 | `engine/health.go:85` | |
| `retireAfterDrop` | 4s | `engine/health.go:90` | |
| `controlInterval` | 3s | `engine/health.go:95` | پینگ کنترل |
| `stuckWait` | 6s | `engine/stuck.go:62` | انتظار پینگ |
| `stuckStreak` | 2 | `engine/stuck.go:63` | |
| `stuckPrompt` | 2s | `engine/stuck.go:69` | شاهد «پاسخ سریع» |
| `stuckRecover` / `stuckRecoverMax` | 30s / 2m | `engine/stuck.go:75-76` | پنجرهٔ بهبود پس از کندی |
| `stuckMoveFloor` | 12KiB/تیک | `engine/stuck.go:82` | |
| `stuckInflate` / `stuckInflateFloor` | 4× / 500ms | `engine/stuck.go:91-92` | تشخیص ازدحام |
| `stuckBaseMins` / `stuckBaseMinN` | 10 دقیقه / 3 | `engine/stuck.go:93-94` | پایهٔ RTT |
| `lossPathMin` | 4 | `engine/loss.go:56` | حداقل لینک قضاوت‌شده برای آزمون «مسیر پراتلاف» |
| `lossWinMin` | 1s | `engine/loss.go:60` | پنجرهٔ دانلود کوتاه‌تر باز می‌ماند |
| `lossQuietKeep` | 20s | `engine/loss.go:64` | نگه‌داشت رگه در نمونهٔ ساکت |
| `lossKeepShare` | 0.5 | `engine/loss.go:67` | لینکی با ≥نیمِ نرخ لینک‌های فشرده نگه داشته می‌شود |
| `ctrlPending` | 4 | `engine/control.go:43` | پینگ بی‌پاسخ |
| `ctrlBusyBytes` | 4KiB | `engine/control.go:47` | «شلوغ» برای پینگ هر تیک و شاهد |

### ۴.۳ refill، دروازه، لاگ

| نام | مقدار | محل |
|---|---|---|
| `refillHoldMax` | 10s | `engine/refill.go:50` |
| `refillStall` | 3s | `engine/refill.go:55` |
| `refillTick` | 100ms | `engine/refill.go:56` |
| `refillRearm` | 1m | `engine/refill.go:57` |
| `refillKeep` | 5m | `engine/refill.go:58` |
| cap در hold | `max(perLink, ceil((users+waiting)/T))` | `engine/refill.go:141-145` |
| `gateInflight` | 8 | `engine/dialgate.go:22` |
| `gatePerSec` | 10 (اسمی) | `engine/dialgate.go:26` |
| `burstLines` / `burstWin` | 8 / 10s | `engine/burstlog.go:18-19` |

### ۴.۴ لایهٔ انتقال و smux

| نام | مقدار | محل | معنی |
|---|---|---|---|
| `TCP_USER_TIMEOUT` | 20000ms | `tlscarrier/tune_linux.go:22,46` | دادهٔ تأییدنشده ⇒ بستن سوکت (هر دو سمت: `tlscarrier/carrier.go:190`, `tlscarrier/server.go:74`) |
| `TCP_NOTSENT_LOWAT` | 32KiB | `tlscarrier/tune_linux.go:19` | |
| کنترل ازدحام | bbr | `tlscarrier/tune_linux.go:29` | |
| TCP keepalive (سمت dial) | 3s | `tlscarrier/carrier.go:187-188` | |
| connect timeout dial | 8s | `tlscarrier/carrier.go:162` | |
| `authTimeout` | 10s | `tlscarrier/auth.go:59` | |
| smux `KeepAliveInterval` | 4–8s تصادفی | `engine/mtcp_link.go:278` | |
| smux `KeepAliveTimeout` | 24s (بستن در ۲۴–۴۸s پس از آخرین قاب، مگر bucket خالی باشد) | `engine/mtcp_link.go:279`؛ smux `session.go:399-422` | |
| smux `openCloseTimeout` | 30s | smux `session.go:17` | SYN/FIN منتظر نویسنده |
| `SmuxFrameSize` / `SmuxStreamBuffer` / `SmuxSessionBuffer` | 16KiB / 2MiB / 8MiB | `engine/mtcp_link.go:258-264` | |
| `relayDieGrace` | 5s | `engine/wedge.go:70` | پس از مرگ جریان |
| `wedgeLooks`/`starveCalls`/`stuckFor`/`guardTick` | 3 / 2048 / 6s / 2s | `engine/wedge.go:48-62` | |
| `l3QueueLen` / `l3MaxSojourn` | 256 / 60ms | `engine/l3_link.go:53,62` | |
| `l3KeepaliveEvery` / `l3DeadAfter` | 2s / 8s | `engine/l3_link.go:67-68` | |
| `l3StreamDeadAfter` / `l3QuietKeepalive` | 30s / 10s | `engine/l3_link.go:78-79` | |
| `l3SessionSilent` | 12s | `engine/l3_link.go:87` | |
| مهلت نوشتن L3 | 5s | `engine/stream.go:215` | |
| `infoTimeout` | 5s | `engine/peerinfo.go:52` | |
| `statsHandshakeTimeout` / `statsReopenAfter` | 5s / 5s | `engine/stats.go:43,100` | |
| `reverseAcceptSlack` / `reverseRefuseHold` | 8 / 5s | `engine/stream_reverse.go:36-37` | |
| `poolCtlInterval` / `poolCtlSlow` / `poolCtlSpread` | 3s / 30s / 1.5s | `engine/exit_pool.go:32,450,466` | |
| `pickWait` | ۴۰×۱۵۰ms ≈ ۶s | `engine/stream_iran.go:184,206` | |

---

## ۵. حلقه‌های کنترلی

| حلقه | ورودی | شرط | خروجی | دوره |
|---|---|---|---|---|
| تیک پول مستقیم (`Run`) | شمارنده‌های meter، TCP_INFO، رکورد خروجی، پینگ کنترل، flowStats | — | reap، حکم‌ها، heal، reconcile، drain، آمار | ۲s |
| تیک پول معکوس (`runAccept`) | همان | — | target برای خروجی، retire، sweep | ۲s |
| autopilot (`decideTarget`) | `apSample` | قواعد کف/probe/shrink | `target` | هر تیک |
| فشار (پیوسته) | dWr/blocked/rwnd و رکورد خروجی | ۲ از ۳ | `pressed` (اولویت pick + سیگنال رشد) | هر تیک |
| suspect | تغییر `rdBytes` | ۱۲s بی‌تغییر | کنار گذاشتن؛ برگشت با اولین بایت | هر تیک |
| loss | TCP_INFO آپلود؛ pong دانلود | `>12%`، ۳ نمونه، calm، نه در «مسیر پراتلاف»، کمتر از نیمِ نرخ مسیر، جا | degraded | تیک/پنجرهٔ pong |
| stuck | `ctrlWait`، شاهدان | ≥۶s، ۲ نمونه، شاهد بعدتر، کم‌جابه‌جایی، نه کند، جا | degraded+stuck | هر تیک |
| مسیر کند | waitingN، شاهدان، پایهٔ RTT، فشار حافظه | ≥۲ منتظر و بیشتر از شاهدان یا تورم ۴× | توقف همهٔ حکم‌ها + پنجرهٔ ۳۰s–۲m | هر تیک |
| تخلیهٔ degraded | `drainSince`, users, stuck | ۰ کاربر / >۴۵s / >۹۰s / سقف | بستن اتصال‌ها/لینک | هر تیک |
| بازنشسته | users، Active، نگهبان‌ها | خالی | بستن (۲ تا ۸ در تیک) | هر تیک |
| بازپس‌گیری بی‌کار | lastActive | ≥ drainIdle (یا جاری‌نبودن پس از ۲۰m) | FIN | هر تیک (یک گذر در لینک) |
| dial | serving<T | gate | لینک تازه | خارج از تیک |
| refill hold | آمدن اولین لینک پس از صفر | target≥2 و نه در ۱m پس از اپیزود محدود | صف اتصال‌ها | ۱۰۰ms، ≤۱۰s |
| کنترل (`openControl`) | تیک ۳s | فعال/سبک/بی‌کار | ping؛ `ctrlWait`، RTT، `peerLoss` | ۳s (فعال)، ۲–۴s (سبک)، ۹–۱۵s (بی‌کار) |
| آمار (`openStats`) | `statsPoll` | لینک ≥16KiB جابه‌جا کرده | رکورد خروجی | بنا به تقاضا |
| wedge guard | Read‌های نشست، نوشتن‌های رله | ۳ نگاه پارک + نوشتن گیر ≥۶s (یا فشار حافظه) | RST رله‌های گیر | ۲s |
| L3 `watchSession` | `rdCalls` نشست | ۱۲s بی‌تغییر | `markDead` کانال L3 | ۲s |
| smux keepalive | دریافت هر قاب | یک دورهٔ کامل ۲۴s بی‌قاب | بستن نشست | NOP ۴–۸s؛ بررسی ۲۴s |
| هسته (TCP_USER_TIMEOUT) | ACK | ۲۰s دادهٔ تأییدنشده | ETIMEDOUT ⇒ watchConn ⇒ بستن نشست | پیوسته |
| pool-control (معکوس) | `ctlTarget` | تغییر/دوره | پیام ۲ بایتی | ۳s (۲ لینک) / ۳۰s |

---

## ۶. حالت‌ها، گذارها، خطاها و بازیابی

### ۶.۱ ماشین حالت `managedLink`

```
                  (dial موفق / AddLink)
                         │
         ┌───────────────▼────────────────┐
         │ تازه: تا infoDone قابل pick نیست │  (gateInfo; حداکثر ~۵s اگر پاسخ نیاید)
         └───────────────┬────────────────┘
       AddLink با S≥T    │
   ┌──── born spare ◄────┤
   │  (retiring+bornSpare)│
   ▼                     ▼
retiring ◄──reconcile shrink── serving ──(pressed: فقط اولویت)
   │ ──reconcile grow/heal──►    │  ▲
   │                             │  │ اولین بایت دریافتی
   │                     12s بی‌دریافت
   │                             ▼  │
   │                          suspect (کنار گذاشته؛ در serving شمرده نمی‌شود)
   │
   ├─ خالی + نگهبان‌ها ⇒ drainTick: بستن (بی‌لاگ «down»)
   │
   └─(loss/stuck verdict)──► degraded ──(همان تیک heal/sweepReverse)──► draining
                                              │
          users==0 ⇒ بستن فوری | >45s یا stuck ⇒ بستن بی‌حرکت‌ها (≥15s) |
          >90s ⇒ بستن با باقی کاربران | سقف+کمبود ⇒ slotsBack (قدیمی‌ترها)
                                              ▼
                                           بسته
هر حالت ──(Alive()=false: TCP/smux/خطای سوکت)──► reap / sweepReverse / DropLink ⇒ بیرون از پول
```

- **degraded برگشت‌ناپذیر است**: هیچ مسیری `degraded` یا `draining` را `false` نمی‌کند (فقط `true`: `engine/loss.go:216`، `engine/linkmanager.go:1987`، و مقداردهی در `heal`/`sweepReverse`).
- **suspect برگشت‌پذیر است**: با اولین تغییر `rdBytes` (`engine/linkmanager.go:1736-1740`). هیچ لاگی برای بازگشت نیست.
- **retiring برگشت‌پذیر است**: `reconcile` (بیشترین open، تازه‌ترین بایت) و `heal` (بیشترین open).

### ۶.۲ چه چیزی لینک را می‌کشد، جایگزین می‌کند یا کنار می‌گذارد

| رویداد | کشتن | کنار گذاشتن از pick | جایگزین | محل |
|---|---|---|---|---|
| خطای خواندن/نوشتن سوکت (شامل TCP_USER_TIMEOUT) | بله (watchConn نشست را می‌بندد) | — | reconcile تیک بعد (مستقیم)؛ خروجی redial (معکوس) | `engine/stream.go:90-123,179-184` |
| smux keepalive (بی‌قاب ۲۴–۴۸s) | بله | — | همان | smux `session.go:409-417` |
| suspect (۱۲s بی‌دریافت) | **خیر** | بله (لایه ۰ و ۱) | مستقیم: reconcile همان تیک dial می‌کند **اگر dialRoom>0** (suspect در max شمرده می‌شود)؛ معکوس: خروجی خودش می‌فهمد | `engine/linkmanager.go:1735-1744`, `773`, `2200-2208` |
| stuck | پس از تخلیه | بله | heal (un-retire یا dial)؛ معکوس: `ctlTarget+1` | `engine/linkmanager.go:1976-1991` |
| loss | پس از تخلیه | بله | همان | `engine/loss.go:180-225` |
| retire (کوچک‌شدن) | وقتی خالی شد | بله (لایه ۰) | — | `engine/linkmanager.go:797-816,959-1056` |
| wedge guard | **خیر**؛ فقط رله‌های گیر را RST می‌کند | — | — | `engine/wedge.go:136-182` |
| کانال L3 `markDead` | فقط جریان L3، نه لینک | جریان‌های TUN به لینک دیگر | — | `engine/l3_link.go:143-189` |
| `infoDone=false` | — | بله (همهٔ لایه‌ها) | — | `engine/linkmanager.go:1554` |
| `pressed` یا کلاهک انفجار | — | فقط اولویت پایین‌تر | سیگنال رشد autopilot | `engine/linkmanager.go:1476-1494` |
| `closeAll` (پایان ctx) | همه | — | — | `engine/linkmanager.go:2374-2382` |

### ۶.۳ خطای dial و بازیابی (مستقیم)

- لاگ دقیق‌حال هر ۳۰s: `want T serving links, only S up — dials failing (peer down or path blocked): <err>` (`engine/linkmanager.go:833-841`).
- هیچ backoff نمایی؛ هر تیک حداکثر `min(ceil(T/4), dialRoom-inflight, ceil(T/4)+8-inflight)` dial جدید؛ در قطعی کامل هر شکست epoch را می‌شکند ⇒ عملاً یک دسته در هر تیک.
- راهنمای short-lived: سه لینک پیاپی با عمر <۲۰s ⇒ یک بار پیشنهاد «tun → icmp» (`engine/linkmanager.go:1440-1462`).

### ۶.۴ خط زمانی خرابی لینک

فرض پایه: لبهٔ مستقیم، `l3mtcp`، max=32 (headroom=4)، per_link=8، لینک L شلوغ، بقیهٔ پول سالم. «t» از لحظهٔ قطع مسیر L.

#### سناریوی A — سیاه‌چاله (همهٔ بسته‌های L در هر دو جهت دور ریخته می‌شوند)

| زمان | رویداد | محل |
|---|---|---|
| t=0 | کاربران L متوقف می‌شوند؛ آپلود تا پر شدن ۳۲KiB notsent در هسته قبول می‌شود، سپس نویسندهٔ smux مسدود | `tlscarrier/tune_linux.go:19` |
| 0–6s | `flowing` لینک L با `flowRecent=6s` به صفر می‌رسد ⇒ در `pickKey` «سبک» دیده می‌شود و **اتصال‌های تازه را جذب می‌کند**؛ محدود به کلاهک انفجار (~۴ اتصال در هر ~۶s) | `engine/mtcp_link.go:117`, `engine/linkmanager.go:1492-1494` |
| ≈3s | پینگ کنترل بعدی L (لینک شلوغ هر ۳s) بی‌پاسخ می‌ماند؛ `ctrlWait` رشد می‌کند | `engine/control.go:102-148` |
| ≈8–13s | **اگر** شاهد سالم شلوغی هست (لینک دیگری پینگ بعدتری را <۲s پاسخ گرفته)، مسیر «کند» نیست، و L کمتر از `max(12KiB, میانه/2)` جابه‌جا کرده ⇒ دو نمونهٔ پیاپی `ctrlWait≥6s` ⇒ لاگ `link N stuck: … — draining`؛ همان تیک `heal`: draining، بازگرداندن یک بازنشسته یا dial جایگزین (replLog) | `engine/linkmanager.go:1884-1889,1976-1991,2089-2131` |
| ≈12–14s | کانال جانبی L3 روی L (۱۲s بدون Read نشست) `markDead` ⇒ جریان‌های TUN آن به لینک‌های دیگر rehash می‌شوند (بسته‌های در راه گم؛ اتصال درونی قطع نمی‌شود) | `engine/l3_link.go:143-161` |
| ≈12–14s پس از آخرین بایت دریافتی | اگر هنوز degraded نشده: `link N: nothing received for 12s — not used for new connections until it answers`؛ از pick کنار می‌رود؛ reconcile همان تیک (اگر جا هست) جایگزین dial می‌کند | `engine/linkmanager.go:1740-1743`, `engine/linkmanager.go:773,845-861` |
| ≈15–17s (فقط مسیر stuck) | گذر `reclaimStalled`: جریان‌هایی که ≥۱۵s بایتی نداشته‌اند بسته می‌شوند (`die` فوراً بسته ⇒ رلهٔ کاربر فوراً تمام؛ FIN خود smux تا ۳۰s منتظر نویسنده) ⇒ کاربران دوباره وصل می‌شوند روی لینک سالم | `engine/linkmanager.go:1225-1236,1361-1386`؛ smux `stream.go:428-439` |
| ≈20–28s | `TCP_USER_TIMEOUT` روی لبه (۲۰s پس از قدیمی‌ترین دادهٔ تأییدنشده؛ NOP smux هر ۴–۸s تضمین می‌کند داده‌ای در راه هست) ⇒ خطای سوکت ⇒ `watchConn` نشست را می‌بندد ⇒ `Alive()=false` | `tlscarrier/tune_linux.go:22`, `engine/stream.go:179-184` |
| +≤۵s | رله‌های باقی‌مانده: `die` ⇒ تا `relayDieGrace=5s` برای تحویل باقی بایت‌ها، سپس بستن اتصال کاربر (FIN) | `engine/wedge.go:325-341` |
| تیک بعد (≤۲s) | `reap`: `mtcp: link N down: <reason>` (احتمالاً `read: timed out (path stalled)` یا `write: …`؛ متن دقیق نامطمئن، بسته به اینکه کدام عمل اول خطا بدهد) ⇒ reconcile اگر serving<T dial می‌کند | `engine/linkmanager.go:1408-1434`, `engine/stream.go:136-161` |
| (smux) ۲۴–۴۸s | اگر TCP زودتر نکشته بود، keepalive smux می‌بندد | smux `session.go:409-417` |
| سمت خروجی | TCP_USER_TIMEOUT خودش (روی دادهٔ تأییدنشدهٔ خودش) و keepalive smux؛ کانال L3 خروجی هم با ۱۲s سکوت جریان‌های برگشتی TUN را جابه‌جا می‌کند؛ تا آن موقع بسته‌های TUN خروجی→لبه که روی L هش شده‌اند گم می‌شوند | `engine/stream_kharej.go:245-253` |

**اثر روی کاربران:** اتصال‌های TCP سنجاق‌شده روی L قطع می‌شوند (در مسیر stuck ≈۱۵–۱۷s، وگرنه ≈۲۰–۳۳s) و اپ باید دوباره وصل شود؛ اتصال‌هایی که در پنجرهٔ ۰ تا ۸–۱۴s روی L جا گرفته‌اند همان سرنوشت را دارند. جریان‌های UDP با دیتاگرام بعدی جریان تازه می‌گیرند. جریان‌های TUN در ≈۱۲–۱۴s بدون قطع اتصال درونی جابه‌جا می‌شوند. اگر **هیچ شاهد سالمی** نباشد (پول ۱–۲ لینکه روی یک مسیر)، مسیر stuck کار نمی‌کند و فقط suspect + مرگ TCP می‌ماند.

#### سناریوی B — همان سیاه‌چاله در حالت معکوس

- suspect در ≈۱۲–۱۴s؛ لبه dial نمی‌کند. لینک suspect در `countsLocked` serving نیست، پس لینک جایگزینِ خروجی «spare» به دنیا نمی‌آید (`engine/linkmanager.go:416-418`).
- خروجی لینک را با TCP_USER_TIMEOUT/keepalive smux خود می‌فهمد؛ slot اگر لینک ≥۳۰s زنده بوده پس از `jitterDur(500ms)` (۲۵۰–۵۰۰ms) و یک نوبت دروازه دوباره dial می‌کند (`engine/exit_pool.go:363-417`).
- اگر stuck حکم شد: `sweepReverse` آن را draining می‌کند و `ctlTarget` یکی بالا می‌رود ⇒ خروجی فوراً جایگزین می‌سازد (لینک‌های fast فوری، بقیه ≤۱.۵s).
- لینک قدیمی وقتی نشستش بسته شد با `DropLink` لاگ می‌شود: `mtcp: reverse link N from IP down: …` (`engine/linkmanager.go:474-496`).

#### سناریوی C — لینک پراتلاف (≥۱۲٪ بازارسال، در حال جابه‌جایی ≥۹۶KiB)

| زمان | رویداد |
|---|---|
| تیک ۱–۳ (آپلود: ۶s؛ دانلود: ۳ پنجرهٔ pong ≈ ۹s+) | رگهٔ بد؛ نمونهٔ ساکت رگهٔ ≤۲۰s را نگه می‌دارد (`engine/linkmanager.go:1832-1849`) |
| حکم | فقط اگر calm (نه مسیر کند/فشار حافظه/پنجرهٔ بهبود)، اکثریت لینک‌های قضاوت‌شده (≥۴) پراتلاف نباشند، لینک <نیمِ میانهٔ نرخ لینک‌های فشرده (وقتی ≥۴ فشرده) جابه‌جا کند، و جا باشد (`drainHeadroom - degradedNow`) ⇒ `link N degraded (up-loss …, down-loss …, moving X Mbit/s where the busy links get Y, rtt …) — draining` |
| همان تیک | `heal`: draining + جایگزین؛ هیچ کاربر تازه |
| کاربر صفر | بستن فوری لینک |
| +۴۵s | `… degraded for 45s — its connections that moved no data for 15s (idle or stuck) are closed now …`؛ هر تیک بعد هم گذر تازه |
| +۹۰s | `… degraded for 1m30s — closed with its N remaining connection(s) …` |
| در سقف | اگر serving+retiring+inflight+room < target ⇒ قدیمی‌ترین draining‌های گذشته از ۴۵s (یا stuck) زودتر بسته: `N degraded link(s) (…) closed with their M remaining connection(s): the pool is at its ceiling …` |

#### سناریوی D — قطعی کامل مسیر (همهٔ لینک‌ها)

- شاهد سالمی نیست ⇒ stuck غیرفعال (`len(answering)==0`)؛ اگر ≥۲ لینک منتظر باشند «مسیر کند» ثبت و تا ۳۰s–۲m پس از آن هیچ حکمی نیست (`engine/linkmanager.go:1935-1950`).
- همهٔ لینک‌ها در ≈۱۲–۱۴s suspect (هر کدام یک خط لاگ، **تاشونده نیست**) ⇒ لایهٔ ۲ `pickLocked` هنوز لینک suspect برمی‌گرداند، یعنی کاربر تازه روی لینک مرده جا می‌گیرد تا مرگ TCP.
- ≈۲۰–۲۸s مرگ TCP؛ `reap` (لاگ‌ها با `burstLog` تا می‌خورند)؛ `exitInfo=nil`؛ dialها در هر تیک شکست می‌خورند، صف با epoch دور ریخته می‌شود؛ هر ۳۰s لاگ «dials failing».
- کاربر تازه بدون لینک: `pickWait` ≈۶s سپس بستن.
- بازگشت مسیر: dial در تیک بعد؛ اولین لینک ⇒ `noteArrivalLocked` ⇒ اپیزود refill (≤۱۰s، cap = سهم منصفانه) ⇒ لاگ `refill: …`. اندازه‌گیری CHANGELOG (آزمون بار Q6): قطعی ۴۰s ⇒ سرویس عادی ۱۶–۲۰s پس از پایان (قبلاً ۶۱s)؛ ری‌استارت ایران زیر بار ⇒ ۱۶.۳s (قبلاً ۷۱s) (`/home/user/hs2-/CHANGELOG.md:812-813`).

#### سناریوی E — throttle DPI (چند بسته در ثانیه، قطع نمی‌شود)

- keepalive گاهی می‌رسد ⇒ هرگز suspect نمی‌شود؛ زیر `activeBytes` ⇒ loss قضاوت نمی‌کند؛ قاعدهٔ stuck دقیقاً برای همین است (`engine/stuck.go:8-15`).
- اندازه‌گیری CHANGELOG: ۱۰ لینک شلوغ گیر ⇒ ۹ تا ۱۰ از ۱۰ در ۱۰.۶s؛ پول کوچک (max 10) با ۴ لینک گیر ⇒ ۹–۱۵s (`/home/user/hs2-/CHANGELOG.md:915-922,1052`).

#### سناریوی F — کوچک‌شدن عادی (نه خرابی)

- retiring ⇒ بی‌کاربر تازه؛ بستن وقتی `users==0 && Active()==0`؛ بازپس‌گیری بی‌کار ≥۳۱۰s؛ پس از ۲۰m جاری‌نبودن‌ها؛ **جریان‌های TUN روی آن هنگام بستن جابه‌جا می‌شوند** (جریان L3 لینک را نگه نمی‌دارد).

---

## ۷. پیام‌های پروتکل و جریان‌های هر لینک

هر جریان smux با یک بایت نوع شروع می‌شود (`engine/stream.go:33-48`):

| kind | مقدار | سازنده | کاربرد در پول |
|---|---|---|---|
| `kindTCP` / `kindTCPPort` | 1 / 8 | `openStream` | اتصال کاربر (با پورت ۲ بایتی اگر خروجی برچسب پورت را می‌فهمد) |
| `kindUDP` / `kindUDPPort` | 2 / 9 | `openStream` | جریان UDP، دیتاگرام `[len u16][payload]` |
| `kindL3` | 3 | `openL3` | TUN، قاب tlscarrier (سرآیند ۷ بایت: نوع، طول ۳ بایت، پد ۳ بایت) (`engine/stream.go:220-238`) |
| `kindCtrl` | 4 | `openControl` | ping `[seq:8][edgeNanos:8]`، pong `[seq:8][edgeNanos:8][exitRetrans:8]` (`engine/control.go:33-36`) |
| `kindPool` | 5 | `openPoolCtl` (فقط معکوس) | جریان پیوستهٔ شمار ۲ بایتی big-endian (`engine/exit_pool.go:29-34`) |
| `kindStats` | 6 | `openStats` | دست‌دهی `[ver]`/`[ver][recLen][caps]`، poll `[seq u32]`، رکورد ۶۴ بایتی (seq, flags شامل chrono/tcp_info/mem-pressure، mono، tx، txBlocked، busy، rwnd، sndbuf، deliveryRate) (`engine/stats.go:24-36`) |
| `kindInfo` | 7 | `openInfo` | یک تبادل: `[ver][n][payload]`؛ v2: maxLinks، caps (`capPortTags`, `capL3Quiet`)، flags، پورت‌ها (`engine/peerinfo.go:22-41`) |

---

## ۸. متن دقیق لاگ‌های مهم و معنی‌شان

(پیشوند بیشترشان `mtcp: ` است؛ خطوطی که از `logs` یا `notes` چاپ می‌شوند پیشوند را هنگام چاپ می‌گیرند.)

| متن | محل | معنی |
|---|---|---|
| `link %d: nothing received for %s — not used for new connections until it answers` | `engine/linkmanager.go:1742` | suspect شد (بازگشتش لاگ ندارد؛ تاشونده نیست) |
| `link %d stuck: its traffic has waited %s for an answer while it moved %s in %s (the other links answer in ~%dms) — draining` | `engine/linkmanager.go:1988` | حکم stuck |
| `link %d degraded (up-loss %v, down-loss %v%s, rtt %dms) — draining` | `engine/loss.go:221` | حکم loss (`%s` = `, moving X Mbit/s where the busy links get Y`) |
| `%d of %d busy links resend more than %.0f%% — the path is lossy, not those links: none is drained` | `engine/loss.go:202` | مسیر پراتلاف؛ حداکثر دقیقه‌ای |
| `%d of %d busy links have waited %s+ for an answer and only %d answer promptly — the path or the other server is slow, not those links: none is drained` | `engine/linkmanager.go:1961` | مسیر کند (شمارشی) |
| `… and the %d that answer promptly take ~%dms, %.0f× their usual ~%dms — the path is congested, not those links: none is drained` | `engine/linkmanager.go:1964` | مسیر ازدحامی (تورم RTT) |
| `kernel TCP memory on %s is above its pressure mark — every socket there is squeezed, not the links: none is judged (look for stalled readers)` | `engine/linkmanager.go:1957` | فشار حافظهٔ هسته |
| `%s %d stuck — its connections that moved no data for %s are closed now (they reconnect onto healthy links); those still moving data stay %s at most` | `engine/linkmanager.go:1228` | پلهٔ تخلیهٔ stuck |
| `%s %d degraded for %s — its connections that moved no data for %s (idle or stuck) are closed now (…); those still moving data (%d active) stay until they end, %s at most` | `engine/linkmanager.go:1233` | پلهٔ ۴۵s |
| `%s %d degraded for %s — closed with its %d remaining connection(s) (they reconnect onto healthy links)` | `engine/linkmanager.go:1223` | سقف ۹۰s |
| `%d degraded %s(s) (%s) closed with their %d remaining connection(s): the pool is at its ceiling and needs their slots (…)` | `engine/linkmanager.go:1309` | پس‌دادن جا |
| `closed %d connection(s) on degraded links that moved no data for %s — they reconnect onto healthy links` | `engine/linkmanager.go:1396` | شمارش دقیقه‌ای |
| `link(s) %s back in service to replace a degraded link` | `engine/linkmanager.go:2128` | un-retire در heal |
| `dialed replacement link %d (make-before-break)` | `engine/linkmanager.go:942` | (replLog) |
| `retired %d drained link(s), now %d` | `engine/linkmanager.go:2180` | |
| `… and %d more degraded link(s) at the same step` | `engine/linkmanager.go:2170` | |
| `link %d down: %s` | `engine/linkmanager.go:1429` | reap (downLog) |
| `reverse link %d from %s down: %s (now %d)` | `engine/linkmanager.go:495` | DropLink |
| `reverse link %d down: %s` / `reverse link %d degraded — draining; the exit is asked for a replacement` / `… (it was retiring: not replaced)` | `engine/linkmanager.go:1155,1161,1163` | sweepReverse |
| `%d links in a row died within %s of coming up — this path lets TCP start and then kills it (…); the tun over icmp transport (tun → icmp) does not use TCP on the wire` | `engine/linkmanager.go:1461` | short-lived |
| `dialed %d link(s) — %d up, %d of %d serving` | `engine/linkmanager.go:831` | |
| `want %d serving links, only %d up — dials failing (peer down or path blocked): %s` | `engine/linkmanager.go:840` | عدد دوم در واقع **serving** است |
| `link(s) %s back in service (%d up: %d serving, %d retiring)` | `engine/linkmanager.go:820` | |
| `link(s) %s retiring — no new connections; each closes once its connections end (…)` | `engine/linkmanager.go:823` | |
| `%s %d retired: its connections ended (%s after retiring) — now %d up (%d serving, %d retiring)` | `engine/linkmanager.go:1029` | (closeLog) |
| `link %d retiring %s: held by %d open connection(s), %d active` / `… empty but kept up — the exit would redial it (…)` | `engine/linkmanager.go:1000,1002` | |
| `closed %d connection(s) on retiring links: %d idle for over %s, %d trickling (not flowing) on links retiring %s+ — …` / `closed %d connection(s) idle for over %s on retiring links` | `engine/linkmanager.go:1050,1053` | |
| `reverse link %d up from %s (now %d)` / `… — spare: the pattern needs %d serving; …` | `engine/linkmanager.go:426-429` | |
| `the exit redials links this server retires (check the exit's min_links) — keeping %d up for %s` | `engine/linkmanager.go:432` | churn |
| `the exit has no pool control (older hs2) — the link count is fixed by its rev_links until it is upgraded` | `engine/linkmanager.go:2018` | |
| `downloads are limited by this server's receive side (…) — more links would not help` | `engine/linkmanager.go:2022` | |
| `refill: %d of %d links up — new connections wait (%s at most) …` و خلاصه‌های پایان | `engine/refill.go:183,332-342` | |
| `+%d more %s in the last %s (latest: %s)` | `engine/burstlog.go:71` | تاخوردن (`links up`، `links down`، `retired links closed`، `replacement links`) |
| `l3: dropped %d packets in 30s on the tun side channel …` | `engine/l3_link.go:389` | دورریز TUN |
| `reset %d connection(s) on %d link(s) whose app had taken nothing for %s while the link's receive buffer was full …` | `engine/wedge.go:272` | wedge |

دلیل‌های `describeNetErr` (`engine/stream.go:136-161`): `closed by the other server`، `closed locally`، `timed out (path stalled)`، `aborted on this server (…)`، `reset by the network or the other server`، `broken pipe (…)`، `network unreachable`.

---

## ۹. گزینه‌های پیکربندی و متغیرهای محیطی مرتبط

| کلید | اثر | محل |
|---|---|---|
| `carrier` | `mtcp`/`l3mtcp`/`l3`/`tls` ⇒ `runStream`؛ `tls` پول را به ۱ سنجاق می‌کند | `cmd/hs2/main.go:405-424,453-...` |
| `min_links` | پیش‌فرض ۲ | `cmd/hs2/main.go:644-657` |
| `max_links` | عدد = ثابت؛ ۰ = خودکار از سخت‌افزار (۳۲/۴۸/۶۴)؛ نبود = ۳۲ | `cmd/hs2/main.go:688-724` |
| `per_link` | پیش‌فرض ۸ | `cmd/hs2/main.go:653-655` |
| `drain_idle_sec` | نبود ⇒ ۳۱۰s؛ ۰ ⇒ هرگز؛ n ⇒ n ثانیه؛ `hs2 check` زیر ۳۰۰ هشدار و منفی خطا | `cmd/hs2/main.go:76,768-777`, `cmd/hs2/check.go:248-250` |
| `reverse` | معکوس | `cmd/hs2/main.go:57` |
| `bind_local_ip` | آی‌پی مبدأ dial | `engine/mtcp_link.go:249` |
| فایل warm | `/run/hs2/…warm`، ۱۵ دقیقه اعتبار | `cmd/hs2/status.go:192-245` |
| `HS2_TUNE_NOTSENT`، `HS2_TUNE_SMUX_FRAME`، `HS2_TUNE_SMUX_STREAMBUF`، `HS2_TUNE_SMUX_SESSBUF`، `HS2_TUNE_CC` | فقط آزمایشگاه | `cmd/hs2/main.go:258-275` |
| `GOMEMLIMIT` | پیش‌فرض نصف RAM | `cmd/hs2/main.go:309-320` |
| `godebug multipathtcp=0` | MPTCP خاموش | `go.mod:7` |

**نکته:** `TCP_USER_TIMEOUT` (۲۰s)، `suspectAfter`، `stuckWait` و دیگر آستانه‌های سلامت هیچ کلید پیکربندی یا متغیر محیطی ندارند.

---

## ۱۰. آزمون‌ها: چه رفتاری تضمین شده

**`engine/linkmanager_test.go`:** `TestPickSpreadsBurst` (۸ اتصال همزمان روی ۴ لینک دقیقاً ۲-۲)، `TestPickReleaseAndDead` (release یک‌باره، مرده‌ها رد)، `TestPickSkipsRetiringDegradedPressed` (ترتیب لایه‌ها و کلاهک انفجار perLink/2)، `TestPickFreshLinkWins`، `TestPickBurstSpreadsWithinTick` (load = flowing+picks؛ نمونه picks را صفر می‌کند)، `TestPickTiesByOpenConnections`، `TestStatsReasonBeforeFirstTick`.

**`engine/pool_v2_test.go` (۳۳ آزمون):**
- فشار: `TestSampleHealthUploadPressure`، `…TwoOfThree`، `TestSampleHealthDownloadPressure`، `…TwoOfThree` (retiring هرگز pressed)، `TestSampleHealthRwndHintLoggedOnce`، `TestSampleHealthStaleRecordNotPressed` (۶s)، `TestSampleHealthUnsupportedStatsNotPressed`، `TestSampleHealthRecordGapRebaselines` (۷s و ری‌استارت خروجی).
- جریان‌ها: `TestFlowStatsFlowingThresholds`، `TestIdleStreamsOnlyLongIdle`.
- reconcile/reap/heal: `TestReconcileUnretiresBeforeDialing`، `TestReconcileVictimOrder`، `TestReconcileStaysWithinCeiling` (max+headroom)، `TestReapRedialsOnlyToTarget` (۵۰۰ کاربر ⇒ فقط ۱ dial)، `TestHealUnretiresInsteadOfDialing`.
- drainTick: `TestDrainTickClosesOnlyEmptyRetiring` (۲ در تیک)، `TestDrainTickBlockingCloseDoesNotStallPick`، `TestDrainTickReclaimSparesBusyStream` (FIN)، `TestReclaimSparesStreamThatWakes`، `TestDrainIdleZeroDisablesReclaim`، `TestDrainTickHeldLogCadence`.
- معکوس: `TestReverseBornRetiringCloseGuards`، `TestReverseChurnGuard`، `TestReversePoolRefusedNoClosesNotGrowable`، `TestOpenPoolCtlDetectsOldExit`.
- آمار: `TestStatsCountsServingRetiring`.
- تخلیه: `TestDegradedLinkDrainsStalledKeepsActive` (۴۵s/۱۵s/۹۰s؛ trickle، مکث <۱۵s، تازه‌بیدار و تازه‌باز حفظ)، `TestStuckDegradedLinkLosesEveryConnection`، `TestStuckDegradedLinkClosesDoNotQueue` (بستن‌ها موازی، نه ۳۰s پشت هم)، `TestReverseRetiringDegradedLinkDrains`، `TestDrainReclaimNotBlockedByRetiringReclaim`، `TestDrainingLinksDoNotBlockRefill`، `TestReverseDegradedLinkReplacedAtOnce` (ctlTarget = 8+1).

**`engine/health_test.go`:** `TestPickSkipsDegraded`، `TestDegradeAndHeal` (۲۰٪ loss ⇒ degraded، یک dial جایگزین، بستن پس از خالی‌شدن)، `TestNoDegradeIdleLink`، `TestDegradeOnDownloadLoss`، `TestNoDegradeHealthy`.

**`engine/stuck_test.go` (۲۰ آزمون):** `TestStuckLinkIsDegraded` (دقیقاً پس از stuckStreak)، `TestStuckNotFlagged` (شامل زیرآزمون **«suspect: nothing received»** — تنها آزمون مستقیم suspect: suspect بدون degraded؛ و «every link waits»)، `TestStuckLinkDrainsAtOnce`، `TestStuckDrainsAtMostHeadroomAtOnce`، `TestStuckMinorityIsCaught` (۴ از ۱۰)، `TestStuckWaitsOutRecoveryAfterMassWait`، `TestStuckRecoveryGrowsWithTheSlowSpell`، `TestStuckBlipsDoNotStretchTheWindow`، `TestStuckSeparateSpellStartsAfresh`، `TestLossWaitsOutASlowSpell`، `TestSlowAnswersAreNotASlowPath`، `TestStuckShareIsHalfTheMedian`، `TestStuckFloorCatchesAThrottleAtNight`، `TestSlowPathNeedsWaitingLightLinks`، `TestStuckCongestedPathIsSlow`، `TestRTTFloorWindow`، و آزمون‌های کانال کنترل (`TestControlWaitShowsUnansweredPing`، `TestControlKeepsWaitingOnAStuckWriter`، `TestCtrlPendingList`، `TestControlWaitFollowsEachAnswer`).

**`engine/loss_test.go`:** `TestDownLossFollowsThePongWindow`، `TestDownLossShortWindowStaysOpen`، `TestUpLossCountsSegments`، `TestLossPathWideDrainsNone`، `TestLossFindsTheLossyAmongThrottledLinks`، `TestLossDrainsAtMostHeadroom`، `TestLossFewBusyLinksJudgedAlone`، `TestLossKeepsALinkAtThePathsRate`، `TestLossStreakSurvivesShortQuietSamples`.

**`engine/refill_test.go`:** `TestRefillHoldSpreadsAfterOutage`، `TestRefillHoldProductionRestart`، `TestRefillHoldSlowRefillNeverRefuses`، `TestRefillHoldOnlyAfterStartOrTotalLoss`، `TestRefillCap`، `TestRefillHoldCancel`، `TestPickWaitRefillHoldLive` (UDP نگه داشته نمی‌شود)، `TestRefillWhySlowTellsPaceFromPath`، `TestRefillHoldReverseStopsAtKharejCeiling`، `TestRefillHoldEndsWhenLinksStopComing`.

**`engine/regress300_test.go`:** `TestRegressDirectRampWithFailedHandshakes` (۱ در ۲۰ شکست، رسیدن به ۱۲۰ در ≤۶۰ تیک)، `TestRegressAcceptCapCountsLiveLinks`، و سه آزمون scout/outage مربوط به `exitPool` (`TestRegressExitFirstLinkSoonAfterOutage` <۳s، `TestOutageScoutUsesItsOwnDial`، `TestExitOutageLoggedAtStartAndEnd`، `TestOutageScoutFirstRetryIsCapped`).

**`engine/shortlived_test.go`:** `TestShortLivedLinksHint`.

**دیگر:** `engine/dialgate_test.go` (`TestDialGatePacesAndBounds`، `TestDialGateCancel`)، `engine/autopilot_scale_test.go` (`TestClosesPerTickScales`، `TestReclaimForcedClosesTricklingNotFlowing`)، `engine/mempressure_test.go` (`TestNoVerdictsUnderMemoryPressure` و…)، `engine/l3_link_test.go` (`TestPickMovesOnlyDeadLinksFlows`، `TestPumpNeverBlocksOnStuckLink`، `TestWriteLoopDropsStalePackets`، `TestStreamL3DroppedWhenSessionSilent`، `TestStreamL3QuietKeepaliveNeedsPeerCap`)، `engine/tun_mode_test.go` (`TestTunModeDirect`، `TestTunModeReverse`، `TestTunModeReverseWithUserPorts`: پورت کاربر روی جریان + TUN روی همان لینک‌ها)، `engine/wedge_test.go`، `engine/control_cadence_test.go`.

**ناربط با LinkManager (برخلاف حدس فهرست وظیفه):** `engine/stage_drop_test.go` (صف حامل dgtun) و `engine/shutdown_test.go` (بستن listener در dgtun).

**بدون پوشش آزمون مستقیم (تا جایی که دیدم):** گذار suspect→serving (بازگشت)، اثر suspect روی `reconcile`/`dialRoom`، رفتار لایهٔ ۲ `pickLocked` با لینک suspect در قطعی کامل، و تعامل TUN با degraded/retiring.

---

## ۱۱. «از قبل وجود دارد» (برای جلوگیری از دوباره‌کاری)

1. پول N لینک TLS واقعی با اثرانگشت Chrome، شکل‌دهی طول جدا، auth پیوسته به TLS exporter.
2. سنجاق‌کردن اتصال به لینک؛ پخش اتصال‌ها با `pickKey` (نافشرده ⇒ کمترین flowing+picks ⇒ کمترین users ⇒ تصادفی).
3. کلاهک انفجار جای‌گذاری (perLink/2 در ۳ نمونه).
4. سه لایهٔ fallback (serving ⇒ retiring ⇒ هر زنده).
5. دروازهٔ تبادل info پیش از اولین کاربر (`gateInfo`) و fallback به `exitInfo`.
6. شروع گرم (۸ یا اندازهٔ پیش از ری‌استارت ≤۱۵ دقیقه).
7. دروازهٔ dial سراسری (≤۸ همزمان، ۴۰–۱۶۰ms)، epoch برای رهاکردن صف، تحمل ۲ شکست پیاپی.
8. make-before-break: un-retire پیش از dial؛ dial جایگزین تا max+⅛.
9. حذف مرده‌ها و پرکردن فقط تا target (نه بر اساس تعداد کاربر).
10. retire قابل برگشت، انتخاب قربانی بر اساس «زودتر خالی‌شدن»، بستن ۲–۸ در تیک با جیتر.
11. بازپس‌گیری بی‌کار روی بازنشسته (۳۱۰s) و اجبار پس از ۲۰m برای trickle.
12. suspect (۱۲s بی‌دریافت).
13. قاعدهٔ loss با رگهٔ جدا برای هر جهت، پنجرهٔ pong، شمارش segment از TCP_INFO، نگه‌داشت رگه در نمونهٔ ساکت، مقایسه با نرخ لینک‌های فشرده، تشخیص «مسیر پراتلاف»، سقف headroom.
14. قاعدهٔ stuck با پینگ کنترل پشت ترافیک خود لینک، شاهد «رفت‌وبرگشت بعدتر»، کف جابه‌جایی، تشخیص مسیر کند (شمارشی + تورم RTT با پایهٔ ۱۰ دقیقه‌ای)، پنجرهٔ بهبود متناسب با طول کندی، استثنای wedge و suspect.
15. توقف همهٔ حکم‌ها زیر فشار حافظهٔ TCP هر دو سرور.
16. تخلیهٔ پله‌ای (۴۵s/۱۵s/۹۰s)، بستن موازی، پس‌دادن جا در سقف، draining خارج از شمارش max.
17. معکوس: born-spare با مهلت ۳۰s، نگهبان churn، تشخیص خروجی قدیمی، سقف پذیرش 2×max+8 روی لینک‌های زنده، درخواست فوری جایگزین (`ctlTarget`)، pool-control سریع روی ۲ لینک.
18. refill hold پس از شروع/قطعی کامل با cap سهم منصفانه.
19. wedge guard، `relayDieGrace`، TCP_USER_TIMEOUT 20s، keepalive smux تصادفی، `watchConn` (بستن نشست با اولین خطا).
20. کانال جانبی L3 با هش rendezvous، صف بی‌انسداد، ماندگاری ۶۰ms، keepalive آرام، رهاسازی با ۱۲s سکوت نشست.
21. لاگ‌های تاشونده و راهنماهای عملیاتی (short-lived، rwnd، no pool control).
22. `PoolStats` افزایشی برای پایش.

---

## ۱۲. ایده‌هایی که امتحان و رد/اصلاح شده‌اند (طبق کد و مستندات)

| ایدهٔ قبلی | مشکل اندازه‌گیری‌شده | جایگزین فعلی | منبع |
|---|---|---|---|
| بستن همهٔ کاربران لینک degraded پس از ۴۵s | ۶۰–۹۰ اتصال فعال در هر لینک قطع | تخلیهٔ پله‌ای | `engine/health.go:72-73` |
| نگه‌داشتن کاربران تا ۵ دقیقه | p90 ۱.۶–۲.۷s تمام آن مدت | سقف ۹۰s | `engine/health.go:67-71`؛ CHANGELOG:842 |
| loss دانلود با pong آخر روی تیک ۲s | ۳۰–۹۰٪ هرگز، ۹٪ با pong لرزان تخلیه | پنجرهٔ بین دو pong | `engine/loss.go:17-25`؛ `engine/health.go:141-148` |
| مخرج بایت/mss | ۱.۱–۱۰× بیش‌برآورد | segment از TCP_INFO | `engine/loss.go:26-29` |
| قضاوت تک‌لینکی بی‌سقف | مسیر پراتلاف همهٔ ۳۰۰ لینک را تخلیه کرد | آزمون مسیر + headroom | `engine/loss.go:31-46`؛ CHANGELOG:1005-1016 |
| مقایسه با میانهٔ لینک‌های شلوغ | لینک‌های پراتلاف میان throttleشده‌ها پنهان شدند | مقایسه با نرخ لینک‌های فشرده | `engine/loss.go:48-51` |
| شمردن لینک‌های کندپاسخ/سنگین/بی‌پاسخ به‌عنوان «مسیر کند» | دو لینک پراتلاف شبانه هر دو قاعده را خاموش کردند | فقط منتظرهای سبک | `engine/stuck.go:39-43`؛ `engine/linkmanager.go:1871-1876` |
| آزمون مسیر کند فقط شمارشی | ازدحام ۱۹۰→۶۰ ۳۳ لینک را stuck کرد | + تورم RTT | `engine/linkmanager.go:1916-1922` |
| قضاوت بلافاصله پس از فشردگی | ۲۷ لینک، ۴۰۰ کاربر قطع | پنجرهٔ بهبود ۳۰s–۲m | `engine/linkmanager.go:1910-1915` |
| پایان کانال کنترل با یک timeout نوشتن | loss کور می‌ماند | ادامه با ≤۴ پینگ منتظر | `engine/control.go:141-148`؛ CHANGELOG:950-957 |
| ضرباهنگ پینگ لرزان برای لینک فعال | هشدار کاذب loss دانلود | ۳s ثابت برای فعال، ۲–۴s برای سبک | CHANGELOG:770؛ `engine/control.go:92-101` |
| شمارش «پاسخ‌دادهٔ دیرتر» به‌عنوان اثبات سلامت | پاسخی که پیش از قطعی رفته بود اثبات حساب شد | «رفت‌وبرگشتی که بعدتر شروع شده» | `engine/linkmanager.go:1968-1975`؛ CHANGELOG:976 |
| cap افزایشی در refill (دوبرابر هر ۲s) | ۱۹۲ روی شلوغ‌ترین لینک | سهم منصفانهٔ ثابت (۹۶) | `engine/refill.go:31-34` |
| بازکردن کل پول در یک انفجار | نشانهٔ رفتاری، CPU، loss | دروازهٔ dial | `engine/dialgate.go:3-16` |
| رهاکردن صف با یک handshake ناموفق | ramp با ۱–۵٪ شکست گیر می‌کرد | ۳ پیاپی یا بدون لینک | `engine/linkmanager.go:196-200`؛ regress300 |
| redial بر اساس کاربران باز | ۵۰۰ کاربر ⇒ ceil(500/8) لینک | فقط تا target | `engine/linkmanager.go:1404-1407` |
| شروع از min | انفجار اتصال پس از شروع روی لینک‌های کم سنجاق می‌شد | شروع گرم ۸ | `engine/health.go:78-84` |
| سقف پذیرش معکوس با لینک‌های مرده | طوفان ۴۶٬۷۲۱ handshake | فقط زنده‌ها | `engine/regress300_test.go:98-100` |
| تشخیص مرگ فقط با keepalive smux | ۱۵s+ معلق | `watchConn` | `engine/stream.go:74-76` |
| رهاسازی کانال L3 پس از ۲۴–۳۰s | | ۱۲s سکوت نشست | README:234 |
| keepalive ثابت ~۵s | اثرانگشت زمانی | ۴–۸s تصادفی | `engine/mtcp_link.go:270-277` |
| pool-control هر ۳s روی همهٔ لینک‌ها | صدها پیام در ثانیه | ۲ سریع + ۳۰s + پخش ۱.۵s | `engine/exit_pool.go:445-466` |
| فشار سنجیده‌شده بدون جبران تأخیر | انفجار روی چند لینک «آزاد» | کلاهک انفجار | `engine/linkmanager.go:1486-1491` |
| شکست stats به‌عنوان «خروجی قدیمی» | لینک در حال مرگ اشتباه گزارش شد | اگر info پاسخ داده، دوباره تلاش | `engine/stats.go:151-157` |
| MPTCP پیش‌فرض Go 1.24 | notsent_lowat نادیده، p99 ۸s | خاموش | CHANGELOG:827-831 |

**مواردی که بررسی شده ولی عمداً تغییر نکرده (CHANGELOG:1085-1094):** هجوم ناگهانی به پول کوچکِ از قبل بالا (refill hold فقط شروع/قطعی کامل)؛ انفجار >۴۰ اتصال در دقیقه برای هر لینک فشار واقعی را از جای‌گذاری پنهان می‌کند (۶–۱۰٪ اتصال‌ها روی لینک throttle)؛ probe autopilot در معکوس سقف خروجی را نمی‌داند. پرسش باز V5: افت بیشتر پس از ازدحام با صف کم‌عمق (`VALIDATION.md:111-118`).

---

## ۱۳. محدودیت‌ها و مشاهده‌ها (بدون پیشنهاد تغییر کد)

- **مشاهده ۱ — TUN مستقل از سلامت پول:** `l3Set.pick` لینک‌های degraded، draining، suspect و retiring را از جریان‌های TUN کنار نمی‌گذارد (`engine/l3_link.go:308-323`)؛ جریان‌های TUN روی لینک پراتلاف تا بسته‌شدن آن (تا ۹۰s) می‌مانند، و سمت خروجی هم مستقل انتخاب می‌کند. در l3mtcp این روی ترافیک hs0 (ping، UDP به 10.77.x) اثر دارد، نه پورت‌های کاربر.
- **مشاهده ۲ — جذب اتصال تازه توسط لینک تازه‌مرده:** پس از قطع، `flowing` در ۶s صفر می‌شود و لینک «سبک‌ترین» به نظر می‌رسد تا suspect (۱۲–۱۴s) یا stuck (۸–۱۳s)؛ فقط کلاهک انفجار محدودش می‌کند (`engine/linkmanager.go:1492-1494`). `pickKey` هیچ ورودی RTT/loss ندارد.
- **مشاهده ۳ — suspect جای max را می‌گیرد:** `dialRoomLocked` لینک suspect را «slotted» می‌شمارد (`engine/linkmanager.go:2200-2207`)؛ پول در سقف تا مرگ TCP جایگزین نمی‌سازد.
- **مشاهده ۴ — لایهٔ ۲ شامل suspect:** در قطعی کامل، `Pick` کاربر تازه را روی لینک suspect می‌گذارد (`engine/linkmanager.go:1560-1570`) به‌جای انتظار در `pickWait`.
- **مشاهده ۵ — لینک «متولد در سیاه‌چاله» هرگز suspect نمی‌شود:** `suspect` نیاز به `rxSeen` دارد (`engine/linkmanager.go:1740`)؛ اگر پس از auth هیچ قاب smux نرسد، پس از `infoTimeout=5s` هم `infoDone=true` می‌شود (`engine/peerinfo.go:291`) و لینک تا مرگ TCP قابل pick است. (احتمال وقوع عملی: نامطمئن.)
- **مشاهده ۶ — شمارش headroom ناهمسان:** loss از `drainHeadroom - degradedNow` (همهٔ degradedها) و stuck از `drainHeadroom - stuckNow` (فقط stuckها) استفاده می‌کند (`engine/loss.go:209`، `engine/linkmanager.go:1981`)؛ در یک تیک مجموع تخلیهٔ همزمان می‌تواند از یک headroom بیشتر شود (تا حدود ۲ برابر؛ قصد طراحی نامطمئن).
- **مشاهده ۷ — degraded برگشت‌ناپذیر:** حکم نادرست جبران نمی‌شود؛ فقط سقف‌ها خسارت را محدود می‌کنند.
- **مشاهده ۸ — dial مستقیم بدون backoff/scout:** هر ۲s دسته‌ای تا ۸ dial (connect ۸s)؛ `DialFrom` ctx نمی‌گیرد (`engine/mtcp_link.go:286-292`).
- **مشاهده ۹ — `heal` در un-retire، suspect را بررسی نمی‌کند** (`engine/linkmanager.go:2105`)؛ ممکن است لینک suspect «جایگزین» شود و reconcile باز هم dial کند.
- **مشاهده ۱۰ — ناهمسانی آمار:** `Stats()` لینک suspect را در `Serving` می‌شمارد (`engine/linkmanager.go:2343-2353`) در حالی که `countsLocked` نمی‌شمارد؛ `PoolStats` شمار suspect/degraded/draining ندارد.
- **مشاهده ۱۱ — لاگ‌ها:** خطوط suspect تاشونده نیستند (در قطعی کامل یک خط برای هر لینک در یک تیک)؛ بازگشت از suspect لاگ ندارد؛ در لاگ «dials failing» عدد «up» در واقع serving است (`engine/linkmanager.go:840`)؛ توضیح `poolCtlFast` بالای `poolCtlLive` افتاده (`engine/linkmanager.go:695-697`)؛ ارجاع `lossOf, stuck.go` در `engine/linkmanager.go:1817` کهنه است.
- **مشاهده ۱۲ — فیلدهای مرده/تشخیصی:** `managedLink.dead`، `mtcpLink.dead` (هرگز true)، `goodput`، `lossFrac` (فیلد)، `linkMeter.stalls` نوشته می‌شوند ولی خوانده نمی‌شوند (`engine/linkmanager.go:232,248,255`، `engine/health.go:106,193`).
- **مشاهده ۱۳ — قفل سراسری:** هر `Pick` قفل نوشتن `m.mu` را می‌گیرد و تا ۳ بار روی همهٔ لینک‌ها می‌چرخد؛ `sampleHealth` کل حلقه را زیر همان قفل اجرا می‌کند. اثر عملی در صدها لینک و نرخ بالای اتصال: نامطمئن (اندازه‌گیری‌ای ندیدم).
- **مشاهده ۱۴ — انتظار OpenStream:** روی لینکی که هنوز علامت نخورده، SYN smux می‌تواند تا `openCloseTimeout=30s` پشت نویسندهٔ مسدود بماند (smux `session.go:526`)؛ `openStream` مهلت جداگانه ندارد.
- **مشاهده ۱۵ — عدم مهاجرت:** هر خرابی لینک برای اتصال‌های TCP کاربر یعنی قطع و اتصال دوباره؛ این عمدی است (جلوگیری از reorder) ولی هزینهٔ اصلی هر خرابی همین است.
- **محدودیت‌های مستند:** هجوم به پول از قبل بالا؛ پنهان‌شدن فشار در انفجار اتصال؛ wedge سمت خروجی از لبه دیده نمی‌شود (`engine/stuck.go:55-60`)؛ در فشردگی‌ای که دور می‌ریزد، لینک‌ها یکی‌یکی قضاوت می‌شوند.

---

## ۱۴. ارجاع به زیرسیستم‌های دیگر

| جهت | زیرسیستم | تعامل |
|---|---|---|
| صدا زده می‌شود از | `engine/stream_iran.go` (`RunIran`, `pickWait`, `openStream`, `serveUserTCP/UDP`) | ساخت، `Run`، `Pick`/`pickHeld`/`cancelHeld`/`releaseFor`، `Stats` |
| صدا زده می‌شود از | `engine/stream_reverse.go` (`acceptReverseLinks`) | `AddLink`، `DropLink`، `alive`، `noteOverCap` |
| صدا زده می‌شود از | `engine/exit_pool.go` (`openPoolCtl`) | `ctlTarget`، `targetChanged`، `poolCtlFast`، `poolCtlLive`، `markPoolRefused` |
| صدا زده می‌شود از | `cmd/hs2/main.go` (`runStream`)، `cmd/hs2/status.go` | پیکربندی، warm، status |
| صدا می‌زند | `engine/autopilot.go` | `newAutopilot`، `ap.decide`، `apSample`/`apLink`، `fmtDur`، `mean`، `mbitps` |
| صدا می‌زند | `engine/refill.go` | `noteArrivalLocked`، `refillNoteLocked` |
| صدا می‌زند | `engine/dialgate.go` | `linkGate.acquireIf` |
| صدا می‌زند | `engine/loss.go`، `engine/stuck.go` | `judgeUpLoss`، `judgeDownLoss`، `lossVerdicts`، `rttFloor`، `stuckRecoverFor` |
| می‌خواند | `engine/health.go` (`linkMeter`)، `engine/control.go` (`ctrlWaitOf`, `ctrlNow`)، `engine/stats.go` (`pollStats`, `statsRec`)، `engine/peerinfo.go` (`peerMax`, `infoDone`)، `engine/wedge.go` (`guard.parkedAt`)، `engine/mempressure.go` | سیگنال‌ها |
| از طریق Link | `engine/mtcp_link.go` (`flowStats`, `idleStreams`, `slowStreams`, `tcpStats`, `downReason`) | |
| لایهٔ زیرین | `engine/stream.go` (`newSession`, `watchConn`)، `tlscarrier` (TCP_USER_TIMEOUT، keepalive، dial) | مرگ واقعی لینک |
| موازی و مستقل | `engine/l3_link.go` (`l3Set`) | TUN در l3mtcp؛ فقط از `OnLink` لینک می‌گیرد |
| سمت مقابل | `engine/stream_kharej.go`، `engine/exit_pool.go` | serveControl/serveStats/serveInfo/servePoolCtl/L3 |
