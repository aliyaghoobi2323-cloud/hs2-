# هستهٔ جریان mtcp: لینک، نشست smux، رله، کنترل

> دامنه: پوشهٔ `hs2-src/engine` در ثبت `0812bc9` (برابر main). مسیرها نسبت به `/home/user/hs2-/hs2-src` نوشته شده‌اند.
> فایل‌های اصلی که کامل خوانده شدند: `engine/stream.go`، `stream_iran.go`، `stream_kharej.go`، `stream_reverse.go`، `mtcp_link.go`، `exit_pool.go`، `shape.go`، `control.go`، `peerinfo.go`، `stats.go`، `exitstats.go`، `engine.go`، `constructors.go`، `carrier.go`، `carrier_tcp.go`، `listen.go`، `rcvbuf_linux.go`، `rcvbuf_other.go`؛ و برای فهم کامل مسیر: `wedge.go`، `health.go`، `l3_link.go`، `dialgate.go`، `burstlog.go`، `mempressure.go`، `routes.go`، بخش‌های مرتبط `linkmanager.go` و `refill.go`، `tlscarrier/{carrier,server,auth,tune_linux}.go`، `obfs/shaper.go`، `cmd/hs2/main.go` و کد منبع `xtaci/smux@v1.5.24` (`session.go`، `stream.go`، `shaper.go`، `mux.go`).
> قرارداد: «مشاهده» یعنی نکته‌ای که من در کد دیدم و ادعای خود کد یا مستندات نیست؛ «نامطمئن» یعنی برداشت تأییدنشده.

---

## ۱. نقش و جایگاه در کل سیستم

### ۱.۱ هستهٔ جریان کدام حامل‌ها را می‌سازد
در `cmd/hs2/main.go:405-424` سه حامل TLS همه به `runStream` (`cmd/hs2/main.go:453`) می‌روند:

| حامل | فراخوانی | تعداد لینک | TUN (hs0) |
|---|---|---|---|
| `mtcp` | `runStream(ctx, fc, false, 0)` | پول تطبیقی `min_links..max_links` | ندارد |
| `l3mtcp` / `l3` | `runStream(ctx, fc, true, 0)` (`main.go:410-411`) | پول تطبیقی | دارد، **به‌عنوان کانال جانبی** |
| `tls` | `runStream(ctx, fc, true, 1)` | دقیقاً ۱ (min=max=1، `main.go:472-474`) | دارد، کانال جانبی |

پس **تونل اصلی کاربر (`l3mtcp`) دقیقاً همان mtcp است به‌اضافهٔ یک جریان smux از نوع `kindL3` روی هر لینک** که بسته‌های IP دستگاه hs0 را جابه‌جا می‌کند (`stream_iran.go:126-128`، `stream_iran.go:419-438`، `stream_kharej.go:245-253`).

### ۱.۲ نکتهٔ حیاتی دربارهٔ «TUN + mtcp»
- اتصال TCP کاربر که به **پورت‌های کاربر** (`forward_ports`) روی سرور ایران وصل می‌شود، روی سرور ایران خاتمه می‌یابد و فقط **بایت‌هایش** روی یک جریان smux می‌رود؛ سرور خارج اتصال TCP خودش را به پنل باز می‌کند. هیچ TCP درون TCP نیست (`stream.go:18-31`).
- اما ترافیکی که از **خود رابط hs0** (10.77.0.x) رد می‌شود، به‌صورت بستهٔ IP درون جریان `kindL3` حمل می‌شود؛ یعنی اگر کسی ترافیک TCP حجیم را از hs0 عبور دهد، **TCP درون TCP** است. طراحی صریحاً این مسیر را «برای ping و ترافیک سبک» می‌داند: صف ۲۵۶ بسته، دور ریختن بسته‌ای که بیش از ۶۰ms در صف مانده، و لاگ `l3: dropped ... the tun is for ping and light traffic; the user ports carry the bulk` (`l3_link.go:49-62`، `l3_link.go:389`؛ README بخش «What changed in v3»). برای ترافیک حجیم روی TUN مسیریابی‌شده، مستندات «tun over udp/icmp» (dgtun) را پیشنهاد می‌کنند.

### ۱.۳ نقش‌ها و جهت
- نقش‌ها ثابت‌اند: لبه (edge، ایران، `mode="dial"`) همیشه **کلاینت smux** است و برای هر اتصال کاربر یک جریان باز می‌کند؛ خروجی (exit، خارج) همیشه **سرور smux** است و جریان‌ها را به پنل تحویل می‌دهد (`stream_reverse.go:16-24`).
- `reverse` فقط این را عوض می‌کند که **چه کسی TCP/TLS را شماره‌گیری می‌کند**: direct = ایران شماره‌گیر و کلاینت TLS؛ reverse = خارج شماره‌گیر و کلاینت TLS، ایران شنونده و سرور TLS (`stream_reverse.go:23-24`، `cmd/hs2/main.go:427`).

### ۱.۴ پشتهٔ لایه‌ها روی یک لینک (از پایین به بالا)

```
TCP  (BBR، TCP_NOTSENT_LOWAT=32KiB، TCP_USER_TIMEOUT=20s، NODELAY؛ شماره‌گیر: keepalive هر 3s)
 └─ TLS 1.3  (کلاینت: uTLS HelloChrome_133؛ سرور: crypto/tls با گواهی واقعی) + احراز دوطرفهٔ مقید به EKM درون TLS
     └─ shapedConn      (shape.go)   — هر نوشتن = چند قاب [dataLen u16][padLen u16][data][pad] با اندازهٔ نمونه‌گیری‌شده
         └─ meteredConn (health.go)  — شمارش بایت خواندن/نوشتن و زمان مسدود شدن نویسنده (فقط وقتی meter داده شود؛ در عمل هر دو طرف)
             └─ watchConn (stream.go) — اولین خطای خواندن/نوشتن ⇒ بستن اتصال و نشست؛ شمارش فراخوانی‌های Read برای نگهبان گیر
                 └─ smux v2 session  (قاب 16KiB، پنجرهٔ جریان 2MiB، سطل نشست 8MiB، keepalive تصادفی 4–8s، timeout 24s)
                     ├─ جریان‌های کاربر: kindTCP / kindTCPPort / kindUDP / kindUDPPort
                     ├─ kindCtrl  (پینگ/پونگ سلامت؛ لبه باز می‌کند)
                     ├─ kindStats (آمار سمت ارسال خروجی؛ لبه باز می‌کند)
                     ├─ kindInfo  (یک‌بار در هر لینک؛ سقف و قابلیت‌ها و پورت‌ها)
                     ├─ kindPool  (فقط reverse: هدف تعداد لینک، لبه → خروجی)
                     └─ kindL3    (فقط وقتی TUN هست: بسته‌های hs0)
```
ساخت این پشته در `newSession` است (`stream.go:172-203`): `newShapedConn` (`stream.go:174`) ← `meteredConn` (`stream.go:175-177`) ← `watchConn` (`stream.go:178-184`) ← `smux.Server`/`smux.Client` (`stream.go:187-191`). هر دو سر از همین تابع استفاده می‌کنند، پس شکل‌دهی متقارن است (`stream.go:163-171`، `shape.go:21-32`).

### ۱.۵ چه چیزی بیرون از این زیرسیستم است (فقط ارجاع)
تصمیم اندازهٔ پول (autopilot)، انتخاب لینک، سلامت/تخلیه/گیرکرده‌ها: `linkmanager.go`، `autopilot.go`، `stuck.go`، `loss.go`، `refill.go`. حامل TLS و احراز: `tlscarrier/`. نمونه‌گیر طول: `obfs/shaper.go`. مسیریابی پورت: `routes.go`. مسیر L3 مشترک: `l3_link.go`. نگهبان گیر: `wedge.go`.

---

## ۲. اجزای اصلی

### ۲.۱ نوع‌ها

| نوع | محل | نقش |
|---|---|---|
| `IranConfig` | `stream_iran.go:18-48` | پیکربندی لبه: `Dialer`، `Min/Max/PerLink`، `ListenIP`، `Ports`، `UDP`، `TUN`، `RevServer/RevListener` (reverse)، `DrainIdle`، `WarmLinks`، `OnStart` |
| `KharejConfig` | `stream_kharej.go:19-61` | پیکربندی خروجی: `Listener/Server` (direct)، `Panel` (= expose)، `Routes` (= port_map)، `TUN`، `RevDial/RevDialScout/RevLinks/RevMin/RevMax` (reverse)، `MaxLinks` (برای گزارش)، و فیلدهای داخلی `peers`، `noRoute`، `traffic` |
| `mtcpLink` | `mtcp_link.go:22-33` | یک لینک لبه: `tls *tlscarrier.Carrier`، `sess *smux.Session`، `active`، `dead`، `mtr *linkMeter`، `why`، `flows map[*countedStream]` |
| `countedStream` | `mtcp_link.go:205-216` | جریان کاربر روی لبه: شمارش بایت (`bytes` اتمیک) + EWMA نرخ و `steady` (فقط نمونه‌گیر) |
| `mtcpDialer` | `mtcp_link.go:246-251` | شماره‌گیر لینک direct؛ یک `sampler` مشترک برای همهٔ لینک‌ها |
| `watchConn` | `stream.go:77-88` | نگهبان خطای سوکت؛ `why`، `rdCalls`، `inRead` |
| `shapedConn` | `shape.go:33-44` | قاب‌بندی دوبارهٔ بایت‌ها به اندازه‌های شبیه HTTPS |
| `linkMeter` | `health.go:103-157` | شمارنده‌های خام لینک (بایت، `wrBlocked`، آمار خروجی، `peerInfo`، `ctrlWait`، `guard` و …) |
| `meteredConn` | `health.go:170-196` | شمارش بایت بالای شکل‌دهنده (یعنی بار واقعی، نه padding) |
| `streamPkt` | `stream.go:206-241` | سازگارکنندهٔ یک جریان smux به `pktConn` برای L3 |
| `udpFlow` | `stream_iran.go:277-284` | یک جریان UDP کاربر روی لبه با صف و نویسندهٔ خودش |
| `exitPool` / `exitSlot` | `exit_pool.go:68-117` | پول پویای شماره‌گیری خروجی در reverse |
| `peerInfo` / `linkPeers` / `PeerRoutes` | `peerinfo.go:64-155` | پیام `kindInfo` و مجموعهٔ لینک‌های زندهٔ خروجی |
| `statsRec` | `stats.go:58-63` | رکورد آمار ارسال خروجی |
| `ctrlPendingList` | `control.go:188-221` | پینگ‌های بی‌پاسخ (حداکثر ۴) |
| `exitTraffic` / `exitConn` / `exitCountedStream` | `exitstats.go:22-205` | شمارش کاربر و توان خروجی برای نمایش |
| `sessGuard` / `relayWatch` / `guardSet` | `wedge.go:73-199` | نگهبان «خوانندهٔ گیرکرده» |
| `l3Link` / `l3Set` | `l3_link.go:90-252` | مسیر L3 (کانال جانبی) |
| `Engine` / `Carrier` / `CarrierDialer` / `CarrierListener` | `engine.go:40-46`، `carrier.go:14-42` | موتور بسته‌ای قدیمی (noise/reality/udp/auto)؛ **در مسیر mtcp/l3mtcp استفاده نمی‌شود** |

### ۲.۲ توابع کلیدی

| تابع | محل | کار |
|---|---|---|
| `RunIran` | `stream_iran.go:56-176` | ساخت `LinkManager`، تنظیم `OnLink`، اجرای پول، باز کردن پورت‌های کاربر |
| `RunKharej` | `stream_kharej.go:81-157` | direct: پذیرش لینک‌ها و سرویس جریان‌ها؛ reverse: `runKharejReverse` |
| `newSession` | `stream.go:172-203` | ساخت پشتهٔ shaped/metered/watch + smux + نگهبان |
| `newSmuxConfig` | `mtcp_link.go:267-284` | پیکربندی smux |
| `newEdgeLink` | `mtcp_link.go:299-307` | پیچیدن حامل TLS به‌عنوان لینک لبه (هم direct هم reverse) |
| `(*mtcpDialer).DialLink` | `mtcp_link.go:286-292` | `tlscarrier.DialFrom` + `newEdgeLink` |
| `acceptReverseLinks` | `stream_reverse.go:47-101` | لبهٔ reverse: پذیرش حامل‌ها، سقف پذیرش، `AddLink`/`DropLink` |
| `runKharejReverse` / `serveReverseLink` | `stream_reverse.go:109-192` | خروجی reverse: پول شماره‌گیر و سرویس هر لینک |
| `serveStream` | `stream_kharej.go:173-257` | توزیع هر جریان ورودی خروجی بر اساس بایت نوع |
| `openStream` / `pickWait` / `userStreamHeader` | `stream_iran.go:183-258` | انتخاب لینک، باز کردن جریان، نوشتن سرآیند |
| `serveUserTCP` / `serveUserUDP` | `stream_iran.go:260-416` | سرویس کاربران TCP/UDP |
| `relayStream` | `wedge.go:295-345` | رلهٔ دوطرفه با نگهبان و مهلت پایان نشست |
| `relay` | `stream.go:58-72` | رلهٔ ساده (در مسیر کاربر استفاده نمی‌شود؛ `stream.go:55-57`) |
| `relayUDPConn` | `stream.go:274-300` | رلهٔ UDP خروجی |
| `openControl` / `serveControl` | `control.go:72-181`، `control.go:235-256` | کانال سلامت |
| `openStats` / `runStats` / `serveStats` / `pollStats` | `stats.go:106-268` | آمار سمت ارسال خروجی |
| `openInfo` / `exchangeInfo` / `serveInfo` | `peerinfo.go:267-324` | تبادل `kindInfo` |
| `openPoolCtl` / `servePoolCtl` | `exit_pool.go:480-584` | کنترل اندازهٔ پول در reverse |
| `openL3` | `stream_iran.go:419-438` | جریان کانال جانبی TUN روی لینک تازه |
| `describeNetErr` / `sessionEndReason` | `stream.go:136-161`، `stream_kharej.go:161-171` | ترجمهٔ خطا به جملهٔ قابل‌فهم اپراتور |
| `ListenReuse` / `listenReuseRcvBuf` | `listen.go:12-43` | شنوندهٔ TCP ساده (نه MPTCP) با `SO_REUSEADDR` |
| `acceptBackoff.wait` | `engine.go:215-239` | عقب‌نشینی حلقهٔ پذیرش (۵ms→۱s) |

### ۲.۳ goroutineها

**روی لبه، برای هر لینک** (از `lm.OnLink`، `stream_iran.go:101-129`؛ خود `OnLink` با `go` صدا زده می‌شود: `linkmanager.go` در `AddLink` و `queueDial`):
1. `openControl` — پینگ/پونگ (`stream_iran.go:102`).
2. `openStats` — آمار خروجی، با بازگشایی (`stream_iran.go:103`).
3. حلقهٔ info — `openInfo` تا جواب، سپس هر یک دقیقه (`stream_iran.go:104-122`).
4. `openPoolCtl` — فقط reverse (`stream_iran.go:123-125`)، به‌اضافهٔ یک goroutine خواننده درون آن (`exit_pool.go:495-512`).
5. L3: `openL3` همگام در `OnLink` اجرا می‌شود و `serveLink` (یک goroutine نویسنده `writeLoop` + خواننده `linkToTun`) و `watchSession` را راه می‌اندازد (`stream_iran.go:432-437`، `l3_link.go:127-129`، `l3_link.go:327-330`).
6. درون smux برای هر نشست: `recvLoop`، `sendLoop`، `shaperLoop`، `keepalive`.

**روی خروجی، برای هر لینک:** در direct یک goroutine برای `Server.Handle` (`stream_kharej.go:126`) که حلقهٔ `AcceptStream` را اجرا می‌کند، یک goroutine بستن نشست با پایان ctx (`stream_kharej.go:133-139`)، و برای هر جریان `go serveStream` (`stream_kharej.go:150`). در reverse همین‌ها درون `serveReverseLink` (`stream_reverse.go:158-174`) و یک goroutine `runSlot` برای هر شکاف (`exit_pool.go:216`).

**برای هر اتصال کاربر:** لبه: `serveUserTCP` + دو کپی‌کننده در `relayStream` (`wedge.go:322-323`). خروجی: `serveStream` + دو کپی‌کننده.

**سراسری فرایند:** `guardSet.run` (یکی برای همه، تیک ۲s؛ `wedge.go:210-226`)، `exitTraffic.run` (`stream_kharej.go:101`)، `l3Set.pumpTun` و `l3Set.logDrops` (`stream_iran.go:93-94`، `stream_kharej.go:90-91`)، حلقهٔ `LinkManager.Run` یا `runAccept` (`stream_iran.go:133`)، `acceptReverseLinks` (`stream_iran.go:135`)، یک حلقهٔ پذیرش برای هر پورت کاربر و یک `serveUserUDP` برای هر پورت UDP (`stream_iran.go:138-173`).

---

## ۳. جریان داده و کنترل، گام‌به‌گام

### ۳.۱ چرخهٔ عمر یک لینک در direct

**لبه (ایران، شماره‌گیر):**
1. حلقهٔ پول (`LinkManager.Run`، `linkmanager.go:546-582`) شماره‌گیری را با `queueDial` صف می‌کند؛ هر شماره‌گیری از دروازهٔ سراسری `linkGate` نوبت می‌گیرد: حداکثر ۸ دست‌دهی هم‌زمان و فاصلهٔ شروع ۴۰–۱۶۰ms (`dialgate.go:22-41`، `linkmanager.go:394-396`، `linkmanager.go:895-955`).
2. `mtcpDialer.DialLink` ← `tlscarrier.DialFrom` (`mtcp_link.go:286-292`):
   - اتصال TCP با timeout ۸s و آدرس مبدأ اختیاری `bind_local_ip` (`tlscarrier/carrier.go:161-182`)؛
   - keepalive هستهٔ TCP هر ۳s (`tlscarrier/carrier.go:186-189`)؛ `tuneTCP`: `NODELAY`، `TCP_NOTSENT_LOWAT=32KiB`، `TCP_USER_TIMEOUT=20000ms`، الگوریتم ازدحام `bbr` یا آنچه برنامهٔ تنظیم انتخاب کرده (`tlscarrier/tune_linux.go:19-50`، `cmd/hs2/main.go:357-358`)؛
   - دست‌دهی uTLS با `HelloChrome_133` و `InsecureSkipVerify` (`tlscarrier/carrier.go:191-197`)، مهلت کل `authTimeout=10s` (`tlscarrier/auth.go:59`)؛
   - الزام TLS 1.3 و استخراج EKM (`tlscarrier/carrier.go:223-231`)؛ فرستادن رکورد احراز کلاینت (padding ۲۸۰–۷۲۰ بایت) و خواندن اثبات سرور (`tlscarrier/carrier.go:206-214`، `tlscarrier/auth.go:62-63`).
3. `newEdgeLink` (`mtcp_link.go:299-307`): ساخت `linkMeter` با `statsPoll` (کانال ظرفیت ۱) و `newSession(car.RawConn(), false, sampler, mtr)`؛ از این لحظه دیگر قاب‌بندی خود حامل TLS استفاده نمی‌شود (`tlscarrier/carrier.go:119-122`).
4. لینک با شناسهٔ یکتای `linkSeq` به `m.links` اضافه و `OnLink` در goroutine صدا زده می‌شود (`linkmanager.go:936-948`).
5. `OnLink` (`stream_iran.go:101-129`): کنترل، آمار، info، (reverse: کنترل پول)، (TUN: L3).
6. **دروازهٔ info:** چون `lm.gateInfo = true` (`stream_iran.go:80`)، `pickLocked` لینکی را که `mtr.infoDone` آن هنوز false است برای کاربر انتخاب نمی‌کند (`linkmanager.go:1554-1556`). `infoDone` با پایان اولین تلاش تبادل info (جواب، رد، یا نرسیدن جواب در ۵s) true می‌شود (`peerinfo.go:272`، `peerinfo.go:291`).
7. از این به بعد لینک اتصال کاربر می‌پذیرد (بستگی به وضعیت serving/retiring/degraded/draining/suspect در `linkmanager.go:299-301`).

**خروجی (خارج، شنونده):**
1. `cfg.Listener.Accept` با `acceptBackoff` (`stream_kharej.go:116-125`)؛ شنونده از `ListenReuse` است (`cmd/hs2/main.go:545-547`).
2. `Server.Handle` (`tlscarrier/server.go:72-148`): `tuneTCP`؛ دست‌دهی با مهلت `handshakeTimeout=10s`؛ خواندن **یک رکورد** اول با مهلت `firstReadTimeout=30s`؛ بررسی رکورد احراز؛ اگر نامعتبر بود ← پروکسی به وب‌سایت پوششی؛ بررسی تکرار nonce؛ نوشتن اثبات سرور؛ سپس `onTunnel`.
3. در callback (`stream_kharej.go:126-155`): `mtr := &linkMeter{}` (بدون `statsPoll`)؛ `newSession(..., true, nil, mtr)` — **نمونه‌گیر طول nil، پس برای هر نشست یک `NewHTTPSLengthSampler` تازه** (`shape.go:53-58`)؛ goroutine بستن نشست با پایان ctx؛ `cfg.peers.add(mtr)`؛ لاگ `link up from %s (now %d)`؛ حلقهٔ `AcceptStream` و `go serveStream(...)` برای هر جریان.
4. پایان: با اولین خطای `AcceptStream`، `sess.Close()`، `car.Close()` و لاگ `link down from %s: <reason> (now %d)` با `sessionEndReason` (`stream_kharej.go:152-154`، `stream_kharej.go:161-171`).

**پایان عمر لینک (هر دو سر):**
- اولین خطای خواندن/نوشتن روی سوکت ⇒ `watchConn.fail` دلیل را ذخیره و در goroutine جداگانه اتصال و نشست را می‌بندد (`stream.go:90-96`، `stream.go:179-184`). توضیح کد: smux خودش فقط با timeout keepalive متوجه اتصال مرده می‌شود (`stream.go:74-76`).
- smux خودش: اگر در پنجرهٔ `KeepAliveTimeout=24s` هیچ سرآیندی خوانده نشود **و** سطل نشست خالی نباشد، نشست را می‌بندد (`smux session.go:399-422`). مشاهده: چون پرچم `dataReady` در هر تیک ۲۴ ثانیه‌ای صفر می‌شود، تشخیص عملاً بین ۲۴ تا ۴۸ ثانیه پس از آخرین داده رخ می‌دهد؛ و وقتی سطل خالی است (خواننده گیر کرده) اصلاً بسته نمی‌شود.
- لبه: `mtcpLink.Alive()` = نشست باز و `dead=false` (`mtcp_link.go:190-195`)؛ `reap` در تیک بعدی (۲s) لینک مرده را برمی‌دارد و `mtcp: link %d down: <reason>` لاگ می‌کند (`linkmanager.go:1408-1432`). دلیل از `mtcpLink.downReason` می‌آید (`mtcp_link.go:38-48`).
- لبه، زودتر از مرگ: اگر `rdBytes` یک لینک (که keepaliveهای smux را هم می‌شمارد) `suspectAfter=12s` تکان نخورد، «مشکوک» می‌شود و کاربر جدید نمی‌گیرد (`linkmanager.go:310-317`، `linkmanager.go:1735-1744`).

### ۳.۲ چرخهٔ عمر لینک در reverse

**لبهٔ reverse (ایران، شنونده):** `acceptReverseLinks` (`stream_reverse.go:47-101`):
1. یک `sampler` مشترک برای همهٔ لینک‌های پذیرفته (`stream_reverse.go:48`).
2. `srv.Handle` همان احراز بالا. سپس **سقف پذیرش**: اگر لینک‌های زنده ≥ `2*max+8` باشد، لینک رد می‌شود؛ ابتدا ۵s نگه داشته می‌شود تا خروجی قدیمی فوراً دوباره شماره نگیرد، و حداکثر هر دقیقه یک خط لاگ با شمارش (`stream_reverse.go:68-83`، `stream_reverse.go:35-41`، `linkmanager.go:671-683`).
3. `newEdgeLink` ← `lm.AddLink(l, from)`؛ اگر پول از قبل به هدف serving رسیده باشد لینک «یدکی» (`bornSpare`، retiring) متولد می‌شود (`linkmanager.go:409-437`).
4. حامل تا بسته شدن نشست نگه داشته می‌شود و سپس فوراً `DropLink` (`stream_reverse.go:93-98`، `linkmanager.go:474-497`).
5. لبهٔ reverse شماره نمی‌گیرد (`lm.accept = true`، `stream_iran.go:64-71`)؛ همان autopilot هدف را تعیین می‌کند و از طریق `kindPool` به خروجی می‌فرستد (`linkmanager.go:1124-1143`).

**خروجی reverse (خارج، شماره‌گیر):** `runKharejReverse` (`stream_reverse.go:109-142`):
1. `[min,max]` از `RevMin/RevMax`؛ اندازهٔ اولیه `RevLinks` = `WarmSize(min,max)` = ۸ محدود به بازه (`cmd/hs2/main.go:529`، `health.go:85`، `linkmanager.go:742-753`).
2. `newExitPool` و `pool.serve = serveReverseLink`؛ `RevDialScout` با connect timeout ۲s برای «پیشاهنگ» قطعی (`cmd/hs2/main.go:534-536`).
3. `pool.setTarget(initial)` شکاف‌ها را می‌سازد.
4. هر شکاف (`runSlot`، `exit_pool.go:326-413`): بررسی `retireIfOver` ← `waitTurn` (در قطعی فقط پیشاهنگ شماره می‌گیرد) ← `gate.acquireIf` با شرط «هنوز لازم است» ← شماره‌گیری (پیشاهنگ با شماره‌گیر کوتاه) ← در خطا `dialFailed` و عقب‌نشینی ← در موفقیت `dialed`، سپس `serveReverseLink` تا پایان لینک ← اگر بالاتر از هدف بود بازنشسته می‌شود وگرنه پس از مکث jitterدار دوباره شماره می‌گیرد.
5. `serveReverseLink` (`stream_reverse.go:148-192`): `newSession(server)`، `peers.add`، حلقهٔ `AcceptStream` با `serveStream(..., pool, mtr)`؛ و اگر لینک بدون خطای سوکت و بدون لغو ctx تمام شد، دلیل را «no data from the edge for 24s (keepalive timeout — path stalled)» می‌نویسد (`stream_reverse.go:177-190`).

### ۳.۳ چرخهٔ عمر یک اتصال TCP کاربر

**لبه:**
1. پذیرش روی پورت کاربر؛ شنونده با `ListenReuse` (TCP ساده، `SO_REUSEADDR`، بدون MPTCP؛ `stream_iran.go:141`، `listen.go:28-43`). خطای گذرای پذیرش ⇒ `acceptBackoff` و ادامه (`stream_iran.go:147-157`).
2. `go serveUserTCP(ctx, c, lm, port)` (`stream_iran.go:159`). نکته: هستهٔ لینوکس دست‌دهی TCP کاربر را پیش از هر چیز کامل کرده است.
3. `openStream(ctx, lm, false, port)` (`stream_iran.go:241-258`): تا ۳ تلاش؛ در هر تلاش:
   - `pickWait(ctx, lm, hold=true)` (`stream_iran.go:183-210`): تا ۴۰ دور؛ `lm.pickHeld()` یا در نبود نگه‌داشت `lm.Pick()`؛ اگر منتظرِ نگه‌داشت بازپرسازی شد، روی کانال آن صبر می‌کند (حداکثر `refillHoldMax=10s`، `refill.go:50`)؛ در غیر این صورت ۱۵۰ms خواب. یعنی بدون نگه‌داشت تا حدود ۶s منتظر لینک می‌ماند.
   - `Pick` زیر قفل لینک را بر اساس کلید `(pressed, flowing+picks, users)` و شکستن تساوی تصادفی انتخاب و `users` را زیاد می‌کند (`linkmanager.go:1514-1593`).
   - `link.OpenStream()` (`mtcp_link.go:59-73`): `sess.OpenStream()` فریم SYN را با کلاس `CLSCTRL` و مهلت `openCloseTimeout=30s` می‌فرستد (`smux session.go:122-167`، `session.go:17`، `session.go:526`)؛ سپس `active++` و ثبت در `flows`.
   - نوشتن سرآیند: ۱ بایت (`kindTCP`) یا ۳ بایت (`kindTCPPort` + پورت u16) طبق `userStreamHeader` (`stream_iran.go:219-237`): اگر info همین لینک می‌گوید خروجی برچسب پورت را مسیریابی می‌کند، برچسب‌دار؛ اگر جواب این لینک هنوز نیامده و رد هم نشده، از `lm.exitInfo` (جوابی که لینک دیگری گرفته) استفاده می‌شود؛ خروجی‌ای که رد کرده هرگز برچسب نمی‌گیرد.
   - هر خطا ⇒ بستن جریان، `release()` و تلاش بعدی روی لینک دیگر.
4. شکست هر سه تلاش ⇒ `user.Close()` (کاربر یک اتصال پذیرفته‌شده می‌بیند که بسته می‌شود).
5. `relayStream(user, st, guardOf(st))` (`stream_iran.go:266-267`، `wedge.go:295-345`):
   - دو کپی‌کننده با بافر ۳۲KiB از `sync.Pool` (`stream.go:53`، `wedge.go:316-323`)؛ پوشش `struct{io.Writer}`/`struct{io.Reader}` جلوی `ReadFrom/WriteTo` را می‌گیرد.
   - جهت جریان→کاربر از `watchedWriter` می‌گذرد تا نگهبان گیر ببیند نوشتن به برنامهٔ کاربر گیر کرده یا نه (`wedge.go:297-310`).
   - هر جهتی تمام شود، هر دو سو بسته می‌شوند (**نیم‌بسته پشتیبانی نمی‌شود**).
   - اگر نشست زیر جریان بمیرد (`GetDieCh`)، کپی‌کنندهٔ جریان→کاربر `relayDieGrace=5s` فرصت دارد بافر را تحویل دهد، بعد هر دو بسته می‌شوند (`wedge.go:64-70`، `wedge.go:311-339`).
6. `release()` شمارش `users` لینک را کم می‌کند (`linkmanager.go:1533-1541`)؛ `countedStream.Close` هم `active` را کم و از `flows` حذف می‌کند (`mtcp_link.go:234-242`).

**خروجی:**
1. `serveStream` (`stream_kharej.go:173-257`): خواندن ۱ بایت نوع با مهلت `kindTimeout=10s`؛ برای `kindTCPPort` دو بایت پورت با همان مهلت.
2. `cfg.routeTable().Target(port)`: نگاشت `port_map` ← در غیر این صورت `expose` ← در غیر این صورت رد و لاگ حداکثر یک بار در دقیقه برای هر پورت/پروتکل (`routes.go:39-46`، `routes.go:132-156`).
3. `net.DialTimeout("tcp", target, 5s)` (`stream_kharej.go:233`). در این فاصله داده‌های کاربر که لبه بلافاصله پس از سرآیند فرستاده، در بافر جریان جمع می‌شوند (تا پنجرهٔ ۲MiB). خطای شماره‌گیری ⇒ بستن جریان (لبه EOF می‌بیند و کاربر را می‌بندد؛ هیچ پیام خطای جداگانه‌ای نیست).
4. `cfg.traffic.open()` و `relayStream(up, exitCountedStream{st, c}, mtr.guard)` (`stream_kharej.go:238-244`). روی خروجی، نگهبان روی نوشتن به **پنل** نظارت می‌کند.

**مسیر بایت‌ها در smux (هر دو جهت):**
- نوشتن: `countedStream.Write` ← `smux Stream.writeV2`: داده تا اندازهٔ پنجرهٔ همتا (۲MiB منهای در راه) به قاب‌های ≤۱۶KiB شکسته می‌شود و **هر قاب منتظر می‌ماند تا روی سوکت نوشته شود** (`smux stream.go:339-425`)؛ صف `shaperLoop` یک هیپ است: اول کلاس `CLSCTRL` (SYN/FIN/NOP)، سپس `CLSDATA` به ترتیب شمارهٔ درخواست (`smux shaper.go:9-14`، `session.go:425-462`)؛ چون هر جریان در هر لحظه یک قاب در صف دارد، جریان‌ها عملاً قاب‌به‌قاب نوبت می‌گیرند (`mtcp_link.go:255-257`). `sendLoop` سرآیند ۸ بایتی و داده را در یک بافر کپی و با یک `conn.Write` می‌نویسد (چون `watchConn` متد `WriteBuffers` ندارد؛ `smux session.go:465-500`).
- زیر آن: `watchConn.Write` ← `meteredConn.Write` (شمارش `wrBytes` و `wrBlocked` اگر نوشتن بیش از ۱ms طول بکشد؛ `health.go:183-196`) ← `shapedConn.Write` که داده را به قاب‌هایی با اندازهٔ نمونه‌گیری‌شده می‌شکند و **هر قاب یک نوشتن جداگانه روی TLS = یک رکورد TLS** (`shape.go:63-103`).
- خواندن: `shapedConn.Read` سرآیند ۴ بایتی و بدنه را می‌خواند و padding را دور می‌ریزد (`shape.go:107-132`) ← `meteredConn` (`rdBytes`) ← `watchConn` (`rdCalls`، `inRead`) ← `recvLoop` smux؛ بار PSH از سطل نشست کم می‌شود (سقف ۸MiB)؛ وقتی سطل ≤۰ است، `recvLoop` دیگر از سوکت نمی‌خواند (`smux session.go:320-336`).
- به‌روزرسانی پنجره: گیرنده وقتی ≥ نصف `MaxStreamBuffer` (۱MiB) مصرف کرد یا در اولین خواندن، قاب `cmdUPD` (کلاس `CLSDATA`) می‌فرستد و خود فراخوانی `Read` تا نوشته شدن آن منتظر می‌ماند (`smux stream.go:139-158`، `stream.go:244-260`).

### ۳.۴ چرخهٔ عمر یک جریان UDP کاربر (فقط با `"udp": true`)
1. یک `ListenPacket("udp")` برای هر پورت و یک `serveUserUDP` (`stream_iran.go:163-171`).
2. حلقهٔ واحد خواندن پورت: کلید = آدرس مبدأ کاربر؛ اگر جریان نیست `udpFlow` با صف ۲۵۶ و `go run(...)` (`stream_iran.go:384-415`).
3. اگر بایت‌های منتظر + این دیتاگرام > ۵۱۲KiB یا صف پر، دیتاگرام دور ریخته می‌شود (`stream_iran.go:406-414`).
4. `run`: `openStream(ctx, lm, true, port)` (بدون نگه‌داشت بازپرسازی) ← نویسنده دیتاگرام‌ها را با قاب `[len u16][payload]` می‌نویسد و خواننده پاسخ‌ها را با `pc.WriteTo` برمی‌گرداند (`stream_iran.go:329-383`، `stream.go:246-267`).
5. بیکاری بیش از `udpIdle=2min` (بررسی هر ۳۰s) ⇒ پایان جریان (`stream_iran.go:307-325`).
6. خروجی: `net.Dial("udp", target)` و `relayUDPConn` (`stream_kharej.go:222-231`، `stream.go:274-300`)؛ خواندن از پنل با مهلت ۲ دقیقه.
7. UDP روی جریان smux یعنی تحویل مطمئن و به‌ترتیب (روی TCP) — بدون نگهبان گیر (`relayUDPConn` از `relayStream` استفاده نمی‌کند).

### ۳.۵ چرخهٔ عمر کانال جانبی L3 (hs0) در l3mtcp
1. هر دو سر یک `l3Set` دارند با `pumpTun` (خواندن از TUN) و `logDrops` (`stream_iran.go:90-95`، `stream_kharej.go:87-92`). TUN در `runStream` با `tun.Open` و MTU پیش‌فرض ۱۳۸۰ باز می‌شود، **بدون offload/دسته‌ای** (`cmd/hs2/main.go:456-466`).
2. لبه در `OnLink` جریان خام (`OpenRawStream`، شمرده‌نشده به‌عنوان کاربر) باز و بایت `kindL3` را می‌نویسد؛ خروجی در `serveStream` همان جریان را به `l3Set` اضافه می‌کند (`stream_iran.go:419-438`، `stream_kharej.go:245-253`).
3. `newStreamL3Link` (`l3_link.go:125-138`): مهلت خواندن ۳۰s (`l3StreamDeadAfter`)؛ keepalive هر ۲s، یا اگر همتا `capL3Quiet` اعلام کرده هر ۱۰s±۲۰٪؛ و `watchSession`: اگر شمارندهٔ `rdCalls` کل نشست ۱۲s تکان نخورد، این L3 مرده اعلام می‌شود (`l3_link.go:82-87`، `l3_link.go:143-161`، `l3_link.go:164-169`).
4. ارسال: `pumpTun` هر بسته را با هش rendezvous (FNV-1a روی آدرس‌ها، پروتکل و پورت‌ها برای IPv4 TCP/UDP؛ برای IPv6 فقط دو آدرس) به یک لینک زنده می‌دهد؛ صف پر یا نبود لینک ⇒ دور ریختن (`l3_link.go:308-357`، `l3_link.go:399-415`).
5. `writeLoop`: هرچه در صف است تا ۱۶KiB در یک نوشتن جمع می‌شود؛ بسته‌ای که بیش از ۶۰ms در صف بوده دور ریخته می‌شود؛ `WriteRaw` روی جریان smux با مهلت نوشتن ۵s؛ **هر خطا ⇒ `markDead`** (`l3_link.go:202-243`، `stream.go:214-218`).
6. دریافت: `linkToTun` قاب‌ها را می‌خواند و فقط `TypeData` را در TUN می‌نویسد؛ پایان ⇒ `markDead` و `set.remove` (`l3_link.go:361-377`، `stream_iran.go:434-437`).

---

## ۴. جدول ثابت‌ها، آستانه‌ها، اندازهٔ بافرها و زمان‌سنج‌ها

### ۴.۱ smux و پشتهٔ لینک
| نام | مقدار | محل | معنی |
|---|---|---|---|
| `Version` | 2 | `mtcp_link.go:269` | پروتکل smux نسخهٔ ۲ (کنترل جریان هر جریان) |
| `SmuxFrameSize` | 16KiB | `mtcp_link.go:258` | بزرگ‌ترین قاب داده؛ بازهٔ نوبت‌گیری جریان‌ها |
| `SmuxStreamBuffer` | 2MiB | `mtcp_link.go:262` | پنجرهٔ دریافت هر جریان |
| `SmuxSessionBuffer` | 8MiB | `mtcp_link.go:264` | سطل مشترک همهٔ جریان‌های یک لینک |
| `KeepAliveInterval` | تصادفی ۴–۸s برای هر نشست | `mtcp_link.go:278` | NOP در هر ضربان (چه پرکار چه بیکار) |
| `KeepAliveTimeout` | 24s | `mtcp_link.go:279` | بستن نشست بدون داده (اگر سطل خالی نباشد) |
| `openCloseTimeout` (smux) | 30s | `smux session.go:17` | مهلت نوشتن SYN/FIN |
| `defaultAcceptBacklog` (smux) | 1024 | `smux session.go:15` | صف پذیرش جریان |
| `maxShaperSize` (smux) | 1024 | `smux session.go:16` | سقف هیپ نوبت‌دهی |
| آستانهٔ ارسال UPD | `MaxStreamBuffer/2` = 1MiB | `smux stream.go:146` | گیرنده پس از مصرف این مقدار پنجره را تمدید می‌کند |
| سرآیند قاب smux | 8 بایت | `smux session.go:486-489` | `[ver][cmd][len u16 LE][sid u32 LE]` |
| `shapeHdrLen` | 4 | `shape.go:47` | سرآیند قاب شکل‌دهنده |
| `shapeMaxFrame` | 32KiB | `shape.go:50` | سقف دفاعی قاب رمزگشایی‌شده |
| توزیع نمونه‌گیر | 1400:0.55، 1200:0.08، 900:0.05، 600:0.05، 400:0.05، 250:0.06، 150:0.06، 80:0.06، 40:0.04 | `obfs/shaper.go:41-42` | اندازهٔ هدف هر رکورد TLS (میانگین محاسبه‌شده ≈ ۹۹۱ بایت) |
| `copyBufs` | 32KiB | `stream.go:53` | بافر کپی هر جهت هر اتصال |
| `kindTimeout` | 10s | `stream.go:51` | مهلت خواندن بایت نوع/پورت در خروجی |
| `relayDieGrace` | 5s | `wedge.go:68-70` | مهلت تحویل بافر پس از مرگ نشست |
| `maxDatagram` | 65535 | `stream.go:244` | سقف دیتاگرام UDP |
| `udpIdle` | 2min | `stream.go:270` | پایان جریان UDP بیکار |
| `udpFlowQueue` / `udpFlowBytes` | 256 / 512KiB | `stream_iran.go:286-289` | صف هر جریان UDP لبه |
| پاک‌سازی جریان‌های UDP | هر 30s | `stream_iran.go:308` | |
| `pickWait` | 40 دور × 150ms | `stream_iran.go:184`، `stream_iran.go:206` | انتظار برای لینک (بدون نگه‌داشت) |
| تلاش `openStream` | 3 | `stream_iran.go:243` | لینک بین Pick و OpenStream ممکن است بمیرد |
| شماره‌گیری پنل TCP در خروجی | 5s | `stream_kharej.go:233` | |
| مهلت نوشتن `streamPkt.WriteRaw` | 5s | `stream.go:215` | نوشتن L3 روی جریان |
| سقف قاب L3 خوانده‌شده | `1<<16` برای داده و pad | `stream.go:227` | |

### ۴.۲ TLS و سوکت
| نام | مقدار | محل | معنی |
|---|---|---|---|
| connect timeout | 8s (پیشاهنگ reverse: 2s) | `tlscarrier/carrier.go:162`، `cmd/hs2/main.go:535` | |
| TCP keepalive شماره‌گیر | 3s | `tlscarrier/carrier.go:186-189` | تشخیص سیاه‌چاله در هسته |
| `NotSentLowat` | 32KiB | `tlscarrier/tune_linux.go:19` | سقف بایت ارسال‌نشده در صف هسته |
| `UserTimeoutMs` | 20000 | `tlscarrier/tune_linux.go:22` | شکست لینک با دادهٔ تأییدنشده |
| `CongestionControl` | `bbr` | `tlscarrier/tune_linux.go:29` | |
| `authTimeout` | 10s | `tlscarrier/auth.go:59` | |
| `handshakeTimeout` (سرور) | 10s | `tlscarrier/server.go:47` | |
| `firstReadTimeout` (سرور) | 30s | `tlscarrier/server.go:40` | |
| `writeTimeout` حامل | 5s | `tlscarrier/carrier.go:27` | فقط برای `WriteRaw/SendFrame` حامل؛ در حالت جریانی `RawConn` آن را دور می‌زند |
| `tcp_notsent_lowat` سراسری | 131072 | `tune/tune.go:352` | برای سوکت‌های کاربر و پنل |

### ۴.۳ کنترل، آمار، info، کنترل پول
| نام | مقدار | محل | معنی |
|---|---|---|---|
| `controlInterval` | 3s | `health.go:95` | تیک کانال کنترل |
| `ctrlPingLen` / `ctrlPongLen` | 16 / 24 | `control.go:39-40` | |
| `ctrlPending` | 4 | `control.go:43` | پینگ بی‌پاسخ حداکثر |
| `ctrlBusyBytes` | 4KiB | `control.go:47` | لینک «فعال» = هر تیک پینگ |
| `activeBytes` | 96KiB | `health.go:25` | لینک «سنگین» = دقیقاً روی ضرب ۳s |
| تأخیر تصادفی لینک فعال سبک | 0–1s | `control.go:124-130` | فاصلهٔ ۲–۴s |
| پرش لینک بیکار | ۲–۴ تیک | `control.go:122` | پینگ هر ۹–۱۵s |
| مهلت خواندن پونگ | `2*controlInterval` = 6s | `control.go:152` | |
| `statsVer` / `statsRecLen` | 1 / 64 | `stats.go:41-42` | |
| `statsHandshakeTimeout` | 5s | `stats.go:43` | مهلت دست‌دهی و نوشتن |
| `statsReopenAfter` | 5s | `stats.go:100` | بازگشایی جریان آمار |
| درخواست آمار | فقط اگر لینک در تیک ≥ `pressMinBytes`=16KiB جابه‌جا کرده | `linkmanager.go:1810-1814`، `linkmanager.go:72` | |
| `statsStale` / `statsGap` | 6s / 7s | `linkmanager.go:75-76` | |
| `infoVer` | 2 | `peerinfo.go:51` | |
| `infoTimeout` | 5s | `peerinfo.go:52` | |
| `infoRetry` / `infoTries` | 10s / 3 | `peerinfo.go:85-86` | |
| `infoSlowRetry` | 1min | `peerinfo.go:89` | |
| `infoMaxPorts` | 125 | `peerinfo.go:60` | پورت‌هایی که در طول u8 جا می‌شوند |
| `poolCtlInterval` | 3s | `exit_pool.go:32` | تازه‌سازی روی دو لینک «سریع» |
| `poolCtlSlow` | 30s | `exit_pool.go:450` | تازه‌سازی روی بقیه |
| `poolCtlSpread` | 1.5s | `exit_pool.go:466` | پخش تغییر روی لینک‌های غیرسریع |
| `jitterAround` | ±20٪ | `exit_pool.go:565-567` | |

### ۴.۴ پول خروجی reverse و پذیرش
| نام | مقدار | محل | معنی |
|---|---|---|---|
| `slotBackoffMin` / `slotBackoffMax` | 500ms / 8s | `exit_pool.go:48-49` | عقب‌نشینی دوبرابرشونده با jitter `[d/2,d)` |
| `scoutBackoffMax` | 2s | `exit_pool.go:54` | فاصلهٔ تلاش پیشاهنگ در قطعی |
| `slotStableAfter` | 30s | `exit_pool.go:55` | لینکی که این‌قدر زنده بوده با کف عقب‌نشینی دوباره شماره می‌گیرد |
| `slotFailLogGap` | 30s | `exit_pool.go:56` | تجمیع لاگ خطا |
| `reverseAcceptSlack` | 8 | `stream_reverse.go:36` | سقف پذیرش = `2*max+8` |
| `reverseRefuseHold` | 5s | `stream_reverse.go:37` | نگه داشتن لینک ردشده |
| `gateInflight` | 8 | `dialgate.go:22` | دست‌دهی هم‌زمان در کل فرایند |
| `jitterGap` | 40–160ms | `linkmanager.go:394-396` | فاصلهٔ شروع شماره‌گیری‌ها |
| `acceptBackoff` | 5ms→1s، لاگ هر دقیقه | `engine.go:215-239` | |

### ۴.۵ نگهبان گیر، آمار خروجی، L3
| نام | مقدار | محل | معنی |
|---|---|---|---|
| `guardTick` | 2s | `wedge.go:62` | |
| `wedgeLooks` | 3 | `wedge.go:48` | ≥۴s پارک‌شدن خوانندهٔ نشست |
| `starveCalls` | 2048 | `wedge.go:52` | کمتر از این فراخوانی Read بین دو نگاه = پارک |
| `stuckFor` | 6s | `wedge.go:55` | رله‌ای که این‌قدر در نوشتن محلی مانده |
| `wedgeLogEvery` | 30s | `wedge.go:57` | |
| `blockedMin` | 1ms | `health.go:53` | فقط انتظار بیشتر از این «مسدود» شمرده می‌شود |
| `suspectAfter` | 12s | `linkmanager.go:317` | |
| `healthTick` | 2s | `health.go:18` | تیک نمونه‌برداری (و `exitTraffic`) |
| `flowTau` / `flowingRate` / `flowRecent` / `flowSteadyRate` | 10s / 2KiB/s / 6s / 256B/s | `health.go:41-48` | «جاری بودن» جریان |
| `peakTicks` | 30 (= ۶۰s) | `exitstats.go:54` | |
| `l3QueueLen` | 256 | `l3_link.go:53` | |
| `l3BatchBytes` | 16KiB | `l3_link.go:56` | |
| `l3MaxSojourn` | 60ms | `l3_link.go:62` | |
| `l3KeepaliveEvery` / `l3DeadAfter` | 2s / 8s | `l3_link.go:67-68` | (حالت غیرجریانی) |
| `l3StreamDeadAfter` / `l3QuietKeepalive` | 30s / 10s | `l3_link.go:78-79` | حالت جریانی |
| `l3SessionSilent` | 12s | `l3_link.go:87` | |
| لاگ دور ریختن L3 | هر 30s | `l3_link.go:381` | |

---

## ۵. حلقه‌های کنترلی

| حلقه | ورودی | شرط | خروجی | دوره |
|---|---|---|---|---|
| `openControl` (`control.go:72-181`) | `rdBytes+wrBytes` لینک، پونگ‌ها | فعال (≥4KiB/تیک) ⇒ هر تیک؛ سنگین (≥96KiB) ⇒ روی ضرب؛ بیکار ⇒ پرش ۲–۴ تیک و فقط اگر پینگ معلق نیست؛ سقف ۴ معلق | `rttMicros`، `peerRetrans`، `peerSeen`، `peerLoss` (رکورد پنجرهٔ بین دو پونگ)، `ctrlWait` (سن قدیمی‌ترین پینگ بی‌پاسخ)، `ctrlAnsweredSent` | ۳s (لینک بیکار ۹–۱۵s) |
| `openStats`/`runStats` (`stats.go:106-198`) | سیگنال `statsPoll` از نمونه‌گیر | فقط وقتی لینک ≥16KiB در تیک جابه‌جا کرده (`linkmanager.go:1812-1814`) و `statsState==OK` | `mtr.peer` (آخرین رکورد) که `consumeRecord` به «فشار دانلود» تبدیل می‌کند (`linkmanager.go:2032-2064`) | حداکثر یک درخواست در تیک ۲s |
| حلقهٔ info لبه (`stream_iran.go:104-122`) | پاسخ `kindInfo` | تا جواب یا رد؛ ۳ تلاش با فاصلهٔ ۱۰s، سپس هر ۱ دقیقه | `peerMax`، `peerInfo`، `lm.exitInfo` | ۱۰s سپس ۱min |
| `openPoolCtl` (`exit_pool.go:480-561`) | `ctlTarget()` = هدف serving + لینک‌های در حال تخلیه با جایگزین (`linkmanager.go:1340-1350`) | ارسال در شروع، روی هر تغییر (دو لینک سریع فوری، بقیه با تأخیر تصادفی ≤1.5s)، و تازه‌سازی دوره‌ای | u16 هدف به خروجی | ۳s±۲۰٪ روی دو کهن‌ترین لینک زنده، ۳۰s±۲۰٪ روی بقیه |
| `servePoolCtl` (`exit_pool.go:571-584`) | u16 از لبه | هر عدد ⇒ `setTarget` (محدود به `bounds()`) | شروع شکاف‌های تازه یا فقط پایین آوردن هدف | با هر پیام |
| `runSlot` (`exit_pool.go:326-413`) | هدف، قطعی، دروازه | بالای هدف ⇒ بازنشستگی؛ در قطعی فقط پیشاهنگ | شماره‌گیری/سرویس/دوباره‌شماره‌گیری | عقب‌نشینی ۰.۵–۸s؛ پیشاهنگ ≤۲s |
| `exitTraffic.run` (`exitstats.go:82-156`) | بایت‌های اتصال‌ها و `rdBytes+wrBytes` لینک‌های زنده | — | `exitSnap` (باز، جاری، نرخ، اوج دوتیکی ۶۰s) | ۲s |
| `guardSet.run` (`wedge.go:220-280`) | `rdCalls`/`inRead` هر نشست و `wseq` هر رله | پارک ≥۳ نگاه و رلهٔ گیر ≥۶s، یا فشار حافظهٔ TCP و رلهٔ گیر | RST به سوکت محلی رله‌های گیر | ۲s |
| `l3Link.watchSession` (`l3_link.go:143-161`) | `rdCalls` نشست | ۱۲s بی‌تغییر | `markDead` | هر `min(2s, 12s/4)` = 2s |
| `l3Link.writeLoop` (`l3_link.go:202-243`) | صف بسته‌ها، زمان‌سنج بیکاری | بستهٔ کهنه‌تر از ۶۰ms دور ریخته می‌شود | یک نوشتن جمعی ≤16KiB یا keepalive | رویدادی |
| `serveUserUDP` پاک‌سازی (`stream_iran.go:307-325`) | `f.last` | بیکاری > ۲min | پایان جریان | ۳۰s |
| keepalive داخلی smux (`smux session.go:399-422`) | `dataReady`، سطل | بدون داده در پنجرهٔ ۲۴s و سطل > ۰ | بستن نشست | ضربان ۴–۸s، بررسی ۲۴s |
| حلقه‌های پذیرش | خطای `Accept` | `net.ErrClosed` یا ctx ⇒ پایان؛ بقیه ⇒ عقب‌نشینی | — | ۵ms→۱s |

---

## ۶. حالت‌ها و گذارها، خطاها و بازیابی

### ۶.۱ حالت‌های یک لینک لبه (در حد این زیرسیستم)
- **زنده** ⇔ `sess` باز و `dead=false` (`mtcp_link.go:190-195`). حالت‌های serving/retiring/degraded/draining/suspect متعلق به `LinkManager` است (`linkmanager.go:299-301`).
- **پیش از info**: کاربر نمی‌گیرد تا `infoDone` (`linkmanager.go:1554-1556`).
- **مرگ**: خطای سوکت (`watchConn`)، timeout keepalive smux، یا بستن عمدی (بازنشستگی، تخلیه، پایان ctx).

### ۶.۲ وضعیت آمار (`statsState`، `stats.go:45-47`)
`statsPending` ⇒ (دست‌دهی موفق) `statsOK` ⇒ (پایان جریان در حالی که لینک زنده است) `statsPending` و بازگشایی پس از ۵s (`stats.go:112-120`).
`statsPending` ⇒ `statsUnsupported` فقط وقتی: خطا EOF یا `ErrUnexpectedEOF` بود (نه timeout)، لینک ۲۰۰ms بعد هنوز زنده است، و خروجی به `kindInfo` جواب **نداده** بود (`stats.go:138-162`). در این حالت حلقه برای همیشه متوقف می‌شود.

### ۶.۳ وضعیت info (`linkMeter`، `health.go:124-134`)
`peerInfo=nil, infoDone=false` ⇒ تلاش ۱: جواب ⇒ `peerInfo` ثبت؛ EOF/نسخهٔ بد ⇒ `infoRefused=true` (قطعی، هرگز برچسب پورت نمی‌خورد)؛ timeout ⇒ `infoDone=true` و تلاش ادامه دارد (`peerinfo.go:267-293`). `lm.exitInfo` وقتی هیچ لینکی نماند پاک می‌شود (`linkmanager.go:487-489`، `linkmanager.go:1420-1424`).

### ۶.۴ حالت‌های پول خروجی reverse
- **عادی** ⇒ (خطای شماره‌گیری با `live==0`) **قطعی**: اولین شکافی که شکست خورده «پیشاهنگ» می‌شود، فقط او با عقب‌نشینی ≤۲s و شماره‌گیر کوتاه تلاش می‌کند، بقیه روی `upCh` منتظرند (`exit_pool.go:259-302`، `exit_pool.go:227-242`، `exit_pool.go:353-356`).
- **قطعی** ⇒ (اولین شماره‌گیری موفق) **عادی**: همه بیدار و از دروازه عبور می‌کنند (`exit_pool.go:306-323`).
- وقتی همهٔ لینک‌ها از دست بروند و هدف بالاتر از اندازهٔ اولیه بوده، هدف به اندازهٔ اولیه برمی‌گردد تا لبه دوباره حرف بزند (`exit_pool.go:420-432`).
- **کوچک شدن**: خروجی هرگز خودش لینکی را نمی‌بندد؛ فقط هدف را پایین می‌آورد. لبه لینک خالی را می‌بندد و شکاف آن لینک چون «بالای هدف» است بازنشسته می‌شود؛ دو گونهٔ لاگ «retired — closed by the edge» در برابر «lost (...) — not redialed» (`exit_pool.go:155-160`، `exit_pool.go:378-397`، `exit_pool.go:418`).
- **سقف**: هدف به `[min, min(max, سقف گزارش‌شدهٔ لبه)]` محدود می‌شود (`exit_pool.go:144-168`).

### ۶.۵ ترجمهٔ خطاها (`describeNetErr`، `stream.go:136-161`)
| خطا | متن |
|---|---|
| `io.EOF` | `closed by the other server` (close_notify یا FIN خالی، قابل تفکیک نیستند؛ `stream.go:125-131`) |
| `net.ErrClosed` | `closed locally` |
| timeout | `timed out (path stalled)` |
| `ECONNABORTED` | `aborted on this server (socket killed, e.g. ss -K or a local firewall)` |
| شامل `connection reset` | `reset by the network or the other server` |
| شامل `broken pipe` | `broken pipe (the other side went away)` |
| `no route to host` / `network is unreachable` | `network unreachable` |
| بقیه | متن خام خطا |
`watchConn` قبل از آن `read: ` یا `write: ` می‌گذارد (`stream.go:92`). `sessionEndReason` اگر خطای سوکتی نبود: `session ended (keepalive timeout or closed by the other server)` (`stream_kharej.go:161-171`).

### ۶.۶ سازوکارهای بازیابی
- **لینکی که بین Pick و OpenStream مرد** ⇒ تا ۳ تلاش روی لینک دیگر (`stream_iran.go:242-257`).
- **نبود لینک** ⇒ تا ~۶s انتظار، یا نگه‌داشت بازپرسازی تا ۱۰s پس از شروع/قطعی کامل (`stream_iran.go:183-210`، `refill.go:10-48`).
- **مرگ نشست زیر رله** ⇒ ۵s مهلت تحویل سپس بستن (`wedge.go:328-339`).
- **خوانندهٔ گیرکرده که سطل ۸MiB را پر کرده** ⇒ RST به اتصال‌هایی که ≥۶s در نوشتن محلی مانده‌اند، تا بقیهٔ لینک جریان بیابد (`wedge.go:14-43`، `wedge.go:136-182`، `wedge.go:298-306`).
- **فشار حافظهٔ TCP هسته** (این سرور، یا روی لبه سرور دیگر از پرچم رکورد آمار) ⇒ همان RST بدون انتظار برای پر شدن سطل (`wedge.go:39-43`، `wedge.go:158`، `mempressure.go:5-36`، `linkmanager.go:1667-1676`).
- **جریان آمار ازدست‌رفته** ⇒ بازگشایی پس از ۵s (`stats.go:106-121`).
- **پونگ دیررس** ⇒ کانال کنترل ادامه می‌دهد؛ پونگ‌ها به ترتیب‌اند و هر پونگ پینگ‌های قبلی را هم پاسخ‌داده حساب می‌کند (`control.go:149-178`، `control.go:183-221`).
- **لینک L3 سیاه‌چاله‌ای** ⇒ پس از ۱۲s سکوت کل نشست، جریان‌های TUN آن به لینک‌های دیگر می‌روند (rendezvous فقط جریان‌های همان لینک را جابه‌جا می‌کند؛ `l3_link.go:305-323`).
- **قطعی کامل در reverse** ⇒ پیشاهنگ؛ ثبت آغاز و پایان قطعی در لاگ.

---

## ۷. پیام‌های پروتکل و قالب قاب‌ها

### ۷.۱ بایت نوع جریان (`stream.go:33-48`)
| مقدار | نام | چه کسی باز می‌کند | بعد از بایت نوع |
|---|---|---|---|
| 1 | `kindTCP` | لبه | بایت‌های خام TCP |
| 2 | `kindUDP` | لبه | دیتاگرام‌ها `[len u16 BE][payload]` (`stream.go:246-267`) |
| 3 | `kindL3` | لبه | قاب‌های L3 `[ftype][len 3B][pad 3B][payload]` (`tlscarrier/carrier.go:34-38`)؛ `TypeData=1`، `TypePing=3` (`core/frame.go:33-37`) |
| 4 | `kindCtrl` | لبه | پینگ/پونگ |
| 5 | `kindPool` | لبهٔ reverse | جریانی از u16 BE |
| 6 | `kindStats` | لبه | دست‌دهی و رکوردها |
| 7 | `kindInfo` | لبه | یک تبادل |
| 8 | `kindTCPPort` | لبه | `[port u16 BE]` سپس TCP |
| 9 | `kindUDPPort` | لبه | `[port u16 BE]` سپس دیتاگرام‌ها |
خروجی برای نوع ناشناخته جریان را می‌بندد (`stream_kharej.go:254-255`) — همین رفتار است که لبه از آن «نسخهٔ قدیمی» را تشخیص می‌دهد (EOF).

### ۷.۲ قاب شکل‌دهنده (زیر smux، درون TLS؛ `shape.go:21-32`)
`[dataLen u16 BE][padLen u16 BE][data][pad صفر]`. نوشتن بزرگ به تکه‌های `target-4` شکسته می‌شود (pad≈0) و تکهٔ کوتاه آخر یا نوشتن کوچک تا اندازهٔ هدف **پر** می‌شود (`shape.go:70-101`). خواننده `total>32KiB` را خطا می‌داند (`shape.go:117-119`). شکل‌دهی بایت‌های روی سیم را عوض می‌کند، پس دو سر باید نسخهٔ هم‌خوان داشته باشند (`shape.go:31-32`).

### ۷.۳ قاب smux v2
سرآیند ۸ بایت little-endian: `[ver=2][cmd][length u16][sid u32]` (`smux session.go:486-489`). فرمان‌ها: SYN، FIN، PSH (داده)، NOP (keepalive)، UPD (`[consumed u32][window u32]`، `smux stream.go:244-260`). شناسهٔ جریان‌های کلاینت (لبه) فرد است (از ۳، ۲تا۲تا؛ `smux session.go:104-110`، `smux session.go:135-140`).

### ۷.۴ کانال کنترل (`control.go:33-36`)
- پینگ لبه→خروجی: `[seq u64][edgeNanos u64]` (۱۶ بایت؛ برای هر پینگ بافر تازه، چون smux نوشتنِ تایم‌اوت‌شده را با اشاره‌گر به بافر در صف نگه می‌دارد؛ `control.go:135-140`).
- پونگ خروجی→لبه: `[seq u64][edgeNanos u64][exitRetrans u64]` (۲۴ بایت). `exitRetrans` از `TCP_INFO` سوکت همین لینک در خروجی است (`control.go:235-256`).

### ۷.۵ آمار (`stats.go:24-35`)
```
edge -> exit  [kindStats=6][ver=1]
exit -> edge  [ver=1][recLen=64][caps]       caps bit0=tcp_info, bit1=meter
edge -> exit  [seq u32]                       برای هر درخواست
exit -> edge  رکورد recLen بایتی:
   0 seq u32 | 4 flags u16 (bit0 chrono valid, bit1 tcp_info ok, bit2 فشار حافظهٔ TCP خروجی)
   6 reserved u16 | 8 monoNs u64 | 16 txBytes u64 | 24 txBlockedNs u64
   32 busyUs u64 | 40 rwndLimUs u64 | 48 sndbufLimUs u64 | 56 deliveryRate u64
```
رکورد بلندتر از ۶۴ پذیرفته می‌شود و فقط پیشوند شناخته‌شده خوانده می‌شود (`stats.go:138`، `stats.go:165`، `stats.go:171-179`). شمارهٔ دنباله بین بازگشایی‌ها ادامه می‌یابد (`stats.go:190-192`).

### ۷.۶ info نسخهٔ ۲ (`peerinfo.go:22-48`)
هر طرف: `[ver][n u8][n bytes]`. بار v2: `maxLinks u16, caps u8, flags u8, count u8, count × port u16`.
- `caps`: bit0 = `capPortTags`؛ bit1 = `capL3Quiet`.
- `flags`: bit0 = لبه: UDP را هم جلو می‌برد / خروجی: پنل پیش‌فرض دارد؛ bit1 = `flagCut` (فهرست پورت‌ها بریده شد).
- خواننده هر نسخهٔ ≥۱ را می‌خواند، فیلدهای اضافه را رد می‌کند (`peerinfo.go:227-257`). هر دو طرف همیشه `capPortTags | capL3Quiet` اعلام می‌کنند (`stream_iran.go:81`، `stream_kharej.go:73`).

### ۷.۷ کنترل پول (`exit_pool.go:29-34`)
پس از بایت نوع: جریانی از اعداد u16 BE (تعداد لینک مطلوب). خروجی هیچ‌وقت روی این جریان نمی‌نویسد؛ خروجی قدیمی آن را فوراً می‌بندد و لبه آن را «رد» ثبت می‌کند (`exit_pool.go:470-479`، `exit_pool.go:495-512`).

### ۷.۸ احراز درون TLS (فقط ارجاع؛ `tlscarrier/auth.go:18-51`)
کلاینت: `nonce(16) | HMAC-BLAKE2s(key,"hs2-auth-v2"||nonce||minute||EKM)[:16] | padlen u16 | pad(280–720)`؛ سرور: `HMAC(key,"hs2-srv-v2"||nonce||EKM)[:16] | padlen | pad(120–480)`.

---

## ۸. متن دقیق لاگ‌های مهم و معنی‌شان

| متن | محل | معنی |
|---|---|---|
| `user port %s open (%s), carried over the link pool` | `stream_iran.go:172` | پورت کاربر باز شد؛ `tcp` یا `tcp+udp` |
| `link up from %s (now %d)` | `stream_kharej.go:142` | خروجی direct: لینک تازه از لبه (بدون تجمیع انفجاری) |
| `link down from %s: %s (now %d)` | `stream_kharej.go:154` | خروجی direct: لینک تمام شد با دلیل |
| `mtcp: refused %d reverse link(s)%s (latest from %s): this server holds at most %d (twice its max_links %d + %d) — check the Kharej server's min_links/max_links` | `stream_reverse.go:74-75` | لبهٔ reverse بالای سقف پذیرش |
| `mtcp: reverse link %d up from %s (now %d)` / `... — spare: the pattern needs %d serving; it takes no connections and closes unless needed` | `linkmanager.go:426-429` | لینک reverse پذیرفته شد / یدکی |
| `mtcp: reverse link %d from %s down: %s (now %d)` | `linkmanager.go:495` | لینک reverse بسته شد |
| `mtcp: link %d down: %s` | `linkmanager.go:1429` | لبهٔ direct: لینک مرده برداشته شد |
| `link %d: nothing received for %s — not used for new connections until it answers` | `linkmanager.go:1742` | لینک مشکوک (≥۱۲s بدون دریافت) |
| `mtcp: exit pool starting %d links (the edge sets the count once it is connected)` | `exit_pool.go:181` | آغاز پول خروجی |
| `mtcp: exit pool target %d links (edge asked; was %d, %d up)` | `exit_pool.go:183` | تغییر هدف به درخواست لبه |
| `mtcp: no link up to the edge — dials fail (%v); one slot keeps trying (every ≤%s), the other %d wait for it` | `exit_pool.go:267-268` | آغاز قطعی |
| `mtcp: exit slot %d dial to edge failed: %v (retry in %s)` / `mtcp: %d dials to the edge failed in the last %s, latest: %v` (+ `— no link up for ...`) | `exit_pool.go:286-292` | خلاصهٔ خطاها هر ۳۰s |
| `mtcp: a link to the edge is back after %s with none up (%d dial(s) failed meanwhile); the other slots redial now` | `exit_pool.go:312-313` | پایان قطعی |
| `mtcp: exit link up to edge (slot %d; now %d)` | `exit_pool.go:371` | (تجمیع انفجاری: بیش از ۸ در ۱۰s ⇒ `+K more exit links up in the last 10s (latest: ...)`، `burstlog.go:17-73`) |
| `mtcp: exit slot %d retired — closed by the edge while above its target (pattern shrinking) (now %d)` | `exit_pool.go:390` | کوچک شدن عادی |
| `mtcp: exit slot %d retired — pool above target (now %d)` | `exit_pool.go:392` | دلیل نامعلوم |
| `mtcp: exit slot %d lost (%s) — not redialed, pool above target (now %d)` | `exit_pool.go:394` | ازدست‌رفتن واقعی بالای هدف |
| `mtcp: exit link down (slot %d: %s; now %d); redial` | `exit_pool.go:401` | ازدست‌رفتن و شماره‌گیری دوباره |
| `no data from the edge for 24s (keepalive timeout — path stalled)` | `stream_reverse.go:188` | دلیل: مسیر ایستاده |
| `mtcp: the other server does not report link stats (older hs2) — download pressure unknown; sizing by activity and upload pressure until it is upgraded` | `stats.go:160` | یک بار در هر فرایند |
| `mtcp: reset %d connection(s) on %d link(s) whose app had taken nothing for %s while the link's receive buffer was full — the links' other connections keep flowing` | `wedge.go:272-273` | نگهبان گیر |
| `mtcp: reset %d connection(s) whose app had taken nothing for %s while kernel TCP memory was above its pressure mark (here or on the other server) — their buffers squeezed every socket` | `wedge.go:267-268` | نگهبان زیر فشار حافظه |
| `mtcp: %d link(s) stopped reading for several seconds with no stuck connection to release (UDP/TUN backlog or a slow panel dial)` | `wedge.go:278` | حداکثر هر ۱۰ دقیقه |
| `l3: dropped %d packets in 30s on the tun side channel (queue limit or no link) — in the TLS modes the tun is for ping and light traffic; the user ports carry the bulk, unaffected` | `l3_link.go:389` | دور ریختن در کانال جانبی |
| `ports: Iran user port %d (%s) has no target on this server — ...` / `ports: a %s connection that does not say its user port ...` | `routes.go:150-154` | رد اتصال بی‌مقصد |
| `%s: accept failed: %v (retrying; check the open-files limit if this repeats)` | `engine.go:233` | خطای پذیرش، هر دقیقه |
| `tls: refused a pre-v2 client (no channel binding) from %s: upgrade the Iran server` | `tlscarrier/server.go:125` | کلاینت قدیمی |

---

## ۹. گزینه‌های پیکربندی و متغیرهای محیطی

### ۹.۱ فیلدهای پیکربندی مرتبط (`cmd/hs2/main.go:36-104`)
| فیلد JSON | اثر در این زیرسیستم |
|---|---|
| `carrier` | `mtcp` / `l3mtcp` (`l3`) / `tls` (`main.go:405-424`) |
| `mode` | `dial` = لبه، `listen` = خروجی (`main.go:469`) |
| `reverse` | چه کسی TLS را شماره می‌گیرد (`main.go:489-500`، `main.go:521-540`) |
| `addr`، `sni`، `shared_key`، `bind_local_ip` | شماره‌گیری لینک (`main.go:500`، `main.go:531-536`) |
| `cert_file`/`key_file`، `backend_addr`، `cover_seed` | سمت سرور TLS (direct: خروجی؛ reverse: لبه) |
| `forward_ports`، `user_listen_ip`، `udp` | پورت‌های کاربر لبه (`main.go:477-479`) |
| `min_links`، `max_links`، `per_link` | پوشش پول (`main.go:644-657`)؛ پیش‌فرض min=2، per=8؛ max: عدد = ثابت، 0 = خودکار از سخت‌افزار، نبودن = ۳۲ |
| `drain_idle_sec` | بستن اتصال بیکار روی لینک بازنشسته (نبودن = ۳۱۰s، ۰ = هرگز؛ `main.go:768-777`) |
| `expose`، `port_map` | جدول مسیریابی خروجی (`main.go:513-515`) |
| `mtu`، `iface`، `local_cidr`، `peer_ip` | TUN در l3mtcp/tls (پیش‌فرض MTU ۱۳۸۰؛ `main.go:456-466`) |
| فایل warm | `WarmLinks` از `/run/hs2/<config>.warm` فقط برای افزایش اندازهٔ شروع (`main.go:280-293`) |

### ۹.۲ متغیرهای محیطی (فقط برای آزمایشگاه؛ `cmd/hs2/main.go:256-274`)
`HS2_TUNE_NOTSENT` (`NotSentLowat`)، `HS2_TUNE_SMUX_FRAME`، `HS2_TUNE_SMUX_STREAMBUF`، `HS2_TUNE_SMUX_SESSBUF`، `HS2_TUNE_CC`. همچنین `HS2_NO_TUNE`، `GOMEMLIMIT` (پیش‌فرض نصف RAM)، `HS2_PPROF`. در `go.mod`: `godebug multipathtcp=0` و در `listen.go:41` `SetMultipathTCP(false)`.

---

## ۱۰. آزمون‌ها: چه چیزی تضمین می‌شود

- `stream_test.go`: `TestStreamTCPAndUDP` (۵ پژواک ۱MiB و ۲۰ دیتاگرام روی ۳ لینک)، `TestStreamRecoversAfterLinksDie` (بستن همهٔ لینک‌ها در خروجی و بازیابی در ۱۵s).
- `tun_mode_test.go`: `TestTunModeDirect`، `TestTunModeReverse`، `TestTunModeReverseWithUserPorts` — l3mtcp در هر دو جهت؛ بسته‌ها بی‌خطا (تا ۵ دور ریختن از ۶۰ مجاز)؛ پورت کاربر و L3 هم‌زمان روی همان لینک‌ها.
- `stream_reverse_test.go`: `TestReverseStreamTCPAndUDP`، `TestReverseStreamRedials`، `TestReverseLinkLogsAreHonest` («now N» هرگز بیش از لینک‌های واقعی و دلیل خالی نیست)، `TestReverseExitPoolFollowsEdgeTarget` (۸ گرم ⇒ ۴ بدون دوباره‌شماره‌گیری، سپس ۷ با دقیقاً ۳ شماره‌گیری)، `TestReverseShrinkKeepsHeldConnection`، `TestReverseShrinkUnderIdleConnsKeepsDownloads`.
- `stream_v2_test.go`: سازگاری نسخه‌های مختلط: `TestMixedOldExitShrinksViaRetireIfOver`، `TestMixedOldEdgeResizesNewExit`، `TestMixedDirectShrinkKeepsHeldConnection`، `TestMixedDirectOldExitShrinks`.
- `exit_reason_test.go`: `TestServeReverseLinkReportsWhyItEnded` (بستن تمیز، RST، FIN خالی، سکوت ۲۴s هر کدام دلیل خودش)، `TestDescribeNetErrReasons`.
- `exit_pool_test.go`: رشد تا هدف، کوچک شدن فقط با بازنشستگی لینک‌های تمام‌شده، تفکیک لاگ کوچک‌شدن از ازدست‌رفتن، `TestServePoolCtlApplies`، ردگیری serving+retiring لبه، مرگ لینک serving با/بی retiring، `TestExitPoolGrowthIsPaced`، `TestExitPoolOutageScoutsAndHoldsInitial`، `TestExitPoolSlotRetiresBeforeDialingAboveTarget`، `TestExitPoolClampsToEdgeCeiling`، `TestPoolCtlSendsOnChangeNotEveryInterval`، `TestPoolCtlFastIsTwoOldestLinks`، `TestPoolCtlFastSkipsEndedLoops`، `TestPoolCtlChangeIsSpread`.
- `regress300_test.go`: اولین لینک پس از قطعی ≤۳s، رمپ direct با ۵٪ دست‌دهی ناموفق، سقف پذیرش فقط لینک‌های زنده، شماره‌گیر جداگانهٔ پیشاهنگ، لاگ آغاز/پایان قطعی، سقف اولین تلاش پیشاهنگ.
- `shape_test.go`: رفت‌وبرگشت بی‌خطا، هیچ رکوردی > ۱۴۰۰، حداقل ۳ اندازهٔ متمایز، سربار شکل‌دهی حجیم < ۱۵٪، padding نوشتن کوچک، `TestNewSessionShapesAnyStreamCarrier`.
- `control_test.go`/`control_cadence_test.go`: `TestControlChannelRoundTrip`، `TestControlPingCadence` (فعال: فاصلهٔ ۳s±۳۰۰ms؛ بیکار: حداکثر ۳ پینگ در ۱۹s)، `TestControlSurvivesLatePong`.
- `peerinfo_test.go`: قالب سیم، تبادل روی smux، فراموشی مقدار لینک‌های رفته، گزارش max واقعی پول در reverse، خروجی قدیمی (EOF ⇒ بدون تکرار)، بی‌جوابی محدود، نسخه‌های مختلط، `TestCapNoteReverseOnly`.
- `stats_test.go`/`stats_linux_test.go`: fallback خروجی قدیمی (یک لاگ در فرایند؛ لینک در حال مرگ «قدیمی» شمرده نمی‌شود)، خواننده کند در برابر سریع (>۶۰٪ مسدود در برابر <۱۰٪)، رکورد بلندتر، چیدمان سیم، `TestStatsIdleLinkSendsNoPolls`، `TestSampleHealthPollsOnlyBusyLinks`، `TestStatsStreamReopensAfterStall`، `TestStatsHandshakeTimeoutIsNotOldExit`، `TestStatsNotOlderWhenExitAnsweredInfo`، `TestExitMeterBlockedBehindSlowReaderTCP`.
- `exitstats_test.go`: `TestExitCountsItsOwnTraffic`، `TestPeakOfSmoothsSpikes`.
- `measure_test.go`: `TestMeteredConnBlockedTime`، `TestFlowStatsFlowingNeedsSustainedRate`.
- `wedge_test.go`/`mempressure_test.go`: رهاسازی خواننده‌های گیر و حفظ خوانندهٔ سالم، نبریدن خوانندهٔ مکث‌کردهٔ تنها، قطره‌خوان پنهان‌کنندهٔ گیر نیست، `TestRelayEndsWhenStreamDies`، رهاسازی زیر فشار حافظه (محلی و همتا).
- `l3_link_test.go`: هش FNV، جابه‌جایی فقط جریان‌های لینک مرده، پمپ هرگز روی لینک گیر نمی‌ایستد، دور ریختن بستهٔ کهنه، `TestStreamL3QuietKeepaliveNeedsPeerCap`، `TestStreamL3DroppedWhenSessionSilent`.
- `routes_test.go`: سرآیند جریان کاربر، دروازهٔ info، مسیریابی پورت سرتاسری در هر دو جهت و نسخه‌های مختلط، `TestExitInfoForgottenWithNoLinks`.
- `listen_linux_test.go`: همهٔ شنونده‌ها TCP ساده‌اند حتی با `GODEBUG=multipathtcp=1`.
- `udp_hol_test.go`: `TestUDPFlowDoesNotHoldUpThePort`.
- `dialgate_test.go`: سقف هم‌زمانی و فاصله، لغو.

---

## ۱۱. «از قبل وجود دارد» (برای جلوگیری از دوباره‌کاری)

1. حذف TCP درون TCP برای پورت‌های کاربر؛ هر اتصال = یک جریان smux؛ خروجی خودش به پنل وصل می‌شود.
2. پول چندلینکی با لینک‌های TLS 1.3 واقعی، اثر انگشت Chrome، احراز دوطرفهٔ مقید به EKM، وب‌سایت پوششی برای کاوشگر.
3. حالت reverse کامل (خروجی شماره می‌گیرد) با همان autopilot روی لبه و کنترل پول از راه `kindPool`.
4. شکل‌دهی طول رکوردهای TLS با توزیع شبه‌HTTPS، متقارن، با padding واقعی برای نوشتن کوچک.
5. keepalive تصادفی ۴–۸s برای هر نشست (ضد اثر انگشت زمانی بین نشست‌ها).
6. تشخیص فوری خطای سوکت (`watchConn`) به‌جای انتظار timeout smux؛ ثبت دلیل به زبان اپراتور.
7. وضعیت «مشکوک» پس از ۱۲s بی‌دریافتی؛ تشخیص سکوت نشست برای L3 در ۱۲s.
8. کانال کنترل per-link: RTT، retransmit سمت دانلود، و «سن قدیمی‌ترین پینگ بی‌پاسخ» برای تشخیص لینک گیرکرده؛ آهنگ متغیر برای لینک‌های سبک/بیکار.
9. آمار سمت ارسال خروجی (فشار دانلود، chrono سوکت، نرخ تحویل، پرچم فشار حافظه) فقط وقتی لینک داده جابه‌جا کرده؛ بازگشایی خودکار.
10. تبادل `kindInfo` v2: سقف هر طرف، قابلیت برچسب پورت و L3 آرام، فهرست پورت‌ها؛ دروازهٔ info پیش از اولین کاربر.
11. مسیریابی بر اساس پورت کاربر با جدول روی خروجی (لبه فقط شمارهٔ پورت را می‌گوید).
12. UDP با صف و نویسندهٔ جداگانه برای هر جریان؛ دور ریختن در صف پر.
13. نگهبان «خوانندهٔ گیرکرده» با RST انتخابی و حالت فشار حافظهٔ TCP؛ مهلت پایان رله پس از مرگ نشست.
14. دروازهٔ سراسری شماره‌گیری (۸ هم‌زمان، فاصلهٔ ۴۰–۱۶۰ms)؛ شماره‌گیری پوچ نوبت نمی‌گیرد.
15. پول خروجی reverse: شکاف‌ها، بازنشستگی بالای هدف (هرگز بستن خودسرانه)، پیشاهنگ قطعی با connect کوتاه، نگه داشتن اندازهٔ اولیه پس از قطعی کامل، محدود به سقف لبه.
16. سقف پذیرش لبهٔ reverse `2*max+8` با شمارش لینک‌های زنده و نگه‌داشت ۵s.
17. کنترل پول: دو لینک سریع، بقیه ۳۰s، پخش تغییر در ۱.۵s، تشخیص خروجی قدیمی.
18. شمارش ترافیک و کاربران روی خروجی (نمایش)، با اوج دوتیکی.
19. تنظیم سوکت لینک: BBR، `NOTSENT_LOWAT=32KiB`، `USER_TIMEOUT=20s`، `NODELAY`، keepalive ۳s؛ شنوندهٔ TCP ساده و `SO_REUSEADDR`.
20. L3: هش rendezvous، صف کوتاه، محدودیت زمان ماندن ۶۰ms، جمع کردن نوشتن‌ها تا ۱۶KiB، keepalive آرام با توافق همتا.
21. تجمیع لاگ‌های پرتکرار (`burstLog`) و لاگ‌های محدودشده در زمان.
22. نگه‌داشت بازپرسازی برای پخش اتصال‌ها پس از شروع/قطعی (در `refill.go`).
23. متغیرهای `HS2_TUNE_*` برای آزمایش بدون ساخت دوباره.

---

## ۱۲. ایده‌هایی که امتحان و رد شده‌اند (طبق کد یا مستندات)

| ایده | نتیجه/دلیل | منبع |
|---|---|---|
| حمل بستهٔ IP (TCP درون TCP) برای `tls` و `l3mtcp` | صف‌های چندثانیه‌ای زیر بار؛ جایش را هستهٔ جریان گرفت و TUN فقط کانال جانبی ماند | README «What changed in v3»، `stream.go:18-31` |
| `NotSentLowat` بزرگ‌تر (۶۴KiB+) یا خاموش | ۶۴KiB+ تأخیر روی لینک کند می‌افزود؛ خاموش ۳ تا ۷ برابر بدتر؛ ۱۶–۳۲KiB بهترین | `tlscarrier/tune_linux.go:12-18` |
| شنوندهٔ MPTCP (پیش‌فرض Go 1.24+) | `tcp_notsent_lowat` را نادیده می‌گیرد؛ کاربر نخوان کل بافر ارسال (۴–۷MB) را نگه می‌داشت؛ echo p99 هشت ثانیه | `listen.go:21-27`، CHANGELOG Q6 |
| آهنگ پینگ کنترل jitterدار روی لینک فعال | هشدار کاذب «اتلاف دانلود» را برگرداند؛ ضرب ثابت ۳s روی لینک فعال ماند | CHANGELOG Q2، `control.go:92-101` |
| پایان کانال کنترل با یک timeout نوشتن پینگ | قاعدهٔ اتلاف را برای همیشه کور می‌کرد؛ اکنون ادامه می‌دهد | CHANGELOG Q7، `control.go:142-147` |
| استفادهٔ دوباره از یک بافر برای پینگ‌ها | پینگ‌های در صف بازنویسی می‌شدند | CHANGELOG Q7، `control.go:135-138` |
| کاوش جداگانهٔ سلامت هر لینک | وجود ندارد؛ به بررسی مشکوک و keepalive smux بسنده شده | `mtcp_link.go:187-189` |
| ارسال تازه‌سازی هدف پول روی همهٔ لینک‌ها در هر بازه | صدها پیام در ثانیه؛ اکنون دو لینک سریع + ۳۰s | CHANGELOG Q2، `exit_pool.go:468-479` |
| رله بدون مهلت پایان نشست | بافر و goroutine برای همیشه نگه داشته می‌شد | `wedge.go:64-67`، `TestRelayEndsWhenStreamDies` |
| تشخیص پارک فقط با «در Read نیست» | خوانندهٔ قطره‌چکان گیر را پنهان می‌کرد ⇒ `starveCalls` | `wedge.go:26-31`، `TestWedgeGuardTricklingReaderDoesNotHideStall` |
| FIN برای اتصال کاربر گیرکرده | دادهٔ نخوانده دقیقه‌ها در هسته می‌ماند ⇒ RST (`SetLinger(0)`) | `wedge.go:298-306` |
| حلقهٔ واحد UDP که خودش می‌نوشت/جریان باز می‌کرد | یک کاربر کند کل پورت را می‌خواباند (در آزمون ۰ از ۲۰۰ دیتاگرام) | `stream_iran.go:270-276`، CHANGELOG Q8 |
| سقف پذیرش با شمارش لینک‌های مرده | طوفان رد (۴۶٬۷۲۱ دست‌دهی) | `regress300_test.go`، CHANGELOG Q2 |
| شکاف‌های بازنشسته در صف دروازه پیش از شکاف‌های لازم | اولین لینک ۱۷–۲۹s دیر | `exit_pool.go:337-348`، `regress300_test.go` |
| دور ریختن همهٔ شماره‌گیری‌های صف با یک دست‌دهی ناموفق | رمپ با ۱–۵٪ خطا می‌ایستاد ⇒ فقط ۳ خطای پیاپی یا خطا بدون لینک زنده | CHANGELOG Q2، `linkmanager.go:919-923` |
| بستن همهٔ کاربران لینک خراب پس از ۴۵s / نگه داشتن تا ۵ دقیقه | اولی ۶۰–۹۰ اتصال فعال را می‌برید؛ دومی p90 ۱.۶–۲.۷s ⇒ گام‌به‌گام تا ۹۰s | `health.go:54-73` |
| قضاوت اتلاف دانلود در تیک ۲s | از زمان‌بندی پونگ پیروی می‌کرد نه اتلاف ⇒ پنجرهٔ بین دو پونگ | `health.go:141-149` |
| مخرج اتلاف = بایت/۱۴۰۰ | قاب‌های کوچک/پرشده ۱.۱ تا ۱۰ برابر پراتلاف‌تر دیده می‌شدند ⇒ شمار قطعه‌ها از `TCP_INFO` | `health.go:207-211`، CHANGELOG Q8 |
| لایهٔ Noise درون TLS | هزینهٔ CPU بدون امنیت بیشتر | `hs2-src/BUILD.md` بخش Security model |
| بازکردن یک‌بارهٔ کل پول | انفجار ClientHelloهای یکسان ⇒ دروازه | `dialgate.go:9-21` |
| طولانی‌تر کردن `firstReadTimeout` تا ۶۰s مثل nginx | goroutine بیشتر زیر سیل کاوشگر | `tlscarrier/server.go:34-39` |
| باز کردن TCP_INFO با یک لایه unwrap | پوشش `prefixConn` آن را صفر می‌کرد ⇒ پیمایش کل زنجیره | `tlscarrier/carrier.go:128-146`، CHANGELOG «Post-review fixes» |
| «older hs2» برای جریان آماری که روی لینک کند تمام شد | گزارش غلط ⇒ اگر info جواب داده، فقط دوباره تلاش | `stats.go:151-157` |

---

## ۱۳. محدودیت‌های شناخته‌شده و مشاهده‌ها

### ۱۳.۱ محدودیت‌هایی که خود مستندات گفته‌اند
- کانال جانبی TUN مسیر حجیم نیست؛ زیر بار دور می‌ریزد (README).
- تا چهار جریانِ منتظر شماره‌گیری کند پنل می‌توانند لینک را تا ۵s نگه دارند؛ نسخهٔ قدیمی سرور دیگر بافرهای بی‌نگهبان دارد (README «What the guard cannot see»).
- هجوم ناگهانی اتصال‌ها روی پولی که از قبل بالاست، لینک‌های اول را شلوغ نگه می‌دارد (نگه‌داشت فقط برای شروع/قطعی کامل است)؛ اتصال برای همهٔ عمر به لینکش سنجاق است (CHANGELOG Q8).
- ۱۰٬۰۰۰ اتصال باز ≈ ۰.۹GB RSS روی هر سرور (بافر کپی ۳۲KB در هر جهت)، ۷۴٪ یک هسته روی ایران در ~۱۹۰Mbit/s در آزمایشگاه (CHANGELOG Q8).
- انفجار اتصال تازه (بیش از ~۴۰ در دقیقه برای هر لینک) می‌تواند فشار واقعی لینک را از انتخاب پنهان کند؛ ۶–۱۰٪ اتصال‌ها روی لینک کُندشده می‌نشینند (CHANGELOG Q8، تغییر داده نشده).
- گیر سمت خروجی (خواننده‌اش پشت پنل کند پارک شده) از لبه دیده نمی‌شود (CHANGELOG Q7).
- `tls` تک‌لینکی است و از سقف هر اتصال بیشتر نمی‌رود (README).

### ۱۳.۲ مشاهده‌ها (از خواندن کد؛ بدون پیشنهاد تغییر)
1. **مشاهده — کانال جانبی L3 هر لینک فقط یک بار باز می‌شود.** `openL3` فقط در `OnLink` صدا زده می‌شود (`stream_iran.go:126-128`) و هیچ حلقهٔ بازگشایی ندارد؛ اگر `WriteRaw` با مهلت ۵s شکست بخورد (`stream.go:215`، `l3_link.go:237-240`) یا `watchSession` ۱۲s سکوت ببیند، L3 آن لینک تا پایان عمر لینک از دست می‌رود، حتی اگر لینک دوباره سالم شود. در یک دورهٔ کندی سراسری مسیر که قاعدهٔ «مسیر کند» جلوی تخلیهٔ لینک‌ها را می‌گیرد، ممکن است L3 همهٔ لینک‌ها بمیرد در حالی که لینک‌ها زنده بمانند؛ آن‌گاه hs0 تا جایگزینی لینک‌ها بی‌لینک است (نامطمئن دربارهٔ احتمال وقوع در میدان؛ ساز و کار از کد قطعی است). در مقایسه، جریان آمار بازگشایی دارد (`stats.go:106-121`).
2. **مشاهده — همین الگو برای کنترل پول و کنترل سلامت.** `openPoolCtl` با هر خطای نوشتن (مهلت ۳s) برای همیشه تمام می‌شود (`exit_pool.go:522-525`)؛ `openControl` با خطای غیر timeout یا خواندن نیمه‌کارهٔ پونگ تمام می‌شود (`control.go:142-158`). هیچ‌کدام بازگشایی ندارند؛ لینک‌های دیگر هدف را می‌رسانند، ولی آن لینک بدون RTT/اتلاف دانلود می‌ماند.
3. **مشاهده — L3 وضعیت پول را نمی‌بیند.** `l3Set.pick` فقط `l3Link.Alive()` را بررسی می‌کند (`l3_link.go:308-323`)؛ لینک degraded/draining/suspect/retiring تا وقتی بسته نشده جریان‌های TUN را حمل می‌کند. روی خروجی هم همین‌طور.
4. **مشاهده — هش IPv6 پورت‌ها را نمی‌بیند** (`l3_link.go:409-410`) و ICMP فقط با دو آدرس هش می‌شود؛ همهٔ جریان‌های دو میزبان IPv6 روی یک لینک می‌روند.
5. **مشاهده — TUN در l3mtcp بدون offload و نوشتن دسته‌ای است** (`tun.Open` در `cmd/hs2/main.go:462`)؛ `tunBatch` و TSO/GRO فقط در dgtun هستند (`engine/tunbatch.go:5-10`). هر بسته یک `write` و یک قاب ۷ بایتی دارد.
6. **مشاهده — هزینهٔ شکل‌دهی.** سقف نمونه‌گیر ۱۴۰۰ است، پس هر ۱۶KiB قاب smux به‌طور میانگین حدود ۱۶–۱۷ رکورد TLS می‌شود (میانگین محاسبه‌شدهٔ نمونه ≈ ۹۹۱ بایت از `obfs/shaper.go:41-42`)؛ هر رکورد = یک `Write` روی TLS، یک مُهر AEAD، یک فراخوانی `crypto/rand` در `randFloat` (`obfs/shaper.go:137-141`) و احتمالاً یک syscall. سربار سیم تخمینی من برای حجیم: ۴ بایت سرآیند + ~۲۲ بایت TLS 1.3 برای هر ~۹۸۷ بایت داده (~۲.۶٪) به‌اضافهٔ padding تکهٔ آخر هر قاب (~۳٪). برای نوشتن‌های کوچک (NOP ۸ بایت، UPD ۱۶، پینگ ۲۴، پونگ ۳۲، کلید SSH) هر نوشتن به‌طور میانگین تا ~۹۹۱ بایت پر می‌شود؛ یعنی ترافیک تعاملی و کنترلی چند ده تا صد برابر بزرگ می‌شود. ارتباط این با «۷۴٪ یک هسته در ۱۹۰Mbit/s» نامطمئن است (اندازه‌گیری‌نشده توسط من).
7. **مشاهده — اندازهٔ رکوردها نسبت به HTTPS معمول.** سرآیند رکورد TLS (شامل طول) روی سیم آشکار است؛ این پیاده‌سازی هیچ رکوردی بزرگ‌تر از ۱۴۰۰ نمی‌سازد، در حالی که کتابخانهٔ TLS خود Go پس از ~۱۲۸KB به رکورد ۱۶KB می‌رسد (`maxPayloadSizeForWrite` در `crypto/tls/conn.go:900-944`). نیز توزیع در هر دو جهت یکسان است (بالا و پایین). این‌که ناظر از این تفاوت‌ها استفاده کند یا نه نامطمئن است؛ توضیح کد می‌گوید HTTPS حجیم «عمدتاً رکورد ~۱۴۰۰» است (`obfs/shaper.go:19-23`).
8. **مشاهده — نیم‌بسته پشتیبانی نمی‌شود.** `relayStream` با پایان هر جهت هر دو را می‌بندد (`wedge.go:315-344`)؛ کلاینتی که پس از ارسال درخواست `shutdown(SHUT_WR)` می‌کند پاسخ را از دست می‌دهد. smux v2 هم Close را کامل انجام می‌دهد (`smux stream.go:428-445`).
9. **مشاهده — شکست شماره‌گیری پنل بی‌صدا است.** لبه پیش از دانستن نتیجهٔ شماره‌گیری پنل داده می‌فرستد؛ اگر پنل در دسترس نباشد کاربر فقط بسته شدن اتصال را می‌بیند (`stream_kharej.go:233-237`). (مزیت: بدون رفت‌وبرگشت اضافه.)
10. **مشاهده — بدترین زمان باز شدن اتصال روی پول بیمار.** هر تلاش `openStream` می‌تواند تا ~۶s در `pickWait` و تا ۳۰s در SYN smux (`openCloseTimeout`) بماند و ۳ تلاش وجود دارد؛ نوشتن سرآیند هم مهلت ندارد (`stream_iran.go:248-250`). کنار گذاشتن لینک‌های suspect/degraded این را کم‌احتمال می‌کند (نامطمئن دربارهٔ رخداد واقعی).
11. **مشاهده — تشخیص keepalive smux بین ۲۴ تا ۴۸ ثانیه طول می‌کشد** (`smux session.go:400-417`) و با سطل خالی هرگز. لبه با «مشکوک» ۱۲s و `TCP_USER_TIMEOUT` ۲۰s این را جبران می‌کند؛ سمت شنوندهٔ TLS (خروجی در direct، لبه در reverse) keepalive هستهٔ ۳s را تنظیم نمی‌کند، چون فقط شماره‌گیر آن را صریحاً می‌گذارد (`tlscarrier/carrier.go:186-189` در برابر `tlscarrier/server.go:72-74`)؛ آن‌جا پیش‌فرض keepalive خود Go برای اتصال پذیرفته‌شده (حدود ۱۵s؛ نامطمئن دربارهٔ مقدار دقیق در Go 1.27) به‌اضافهٔ `USER_TIMEOUT`، keepalive smux و خطای سوکت می‌ماند.
12. **مشاهده — UPD در کلاس داده.** قاب تمدید پنجره کلاس `CLSDATA` دارد و `Read` گیرنده تا نوشته شدنش منتظر می‌ماند (`smux stream.go:244-260`، `stream.go:152-156`)؛ روی لینکی با آپلود سنگین، خوانندهٔ دانلود پشت قاب‌های داده (حداکثر یکی برای هر جریان نویسنده) می‌ایستد.
13. **مشاهده — سقف توان هر جریان = پنجره/RTT.** با پنجرهٔ ۲MiB، در RTT ۱۰۰ms حدود ۱۶۰Mbit/s و در ۳۰۰ms حدود ۵۵Mbit/s برای یک اتصال (محاسبهٔ من). سطل ۸MiB یعنی حداکثر ۴ جریان کاملاً پر روی یک لینک.
14. **مشاهده — نگهبان گیر UDP و L3 را پوشش نمی‌دهد** (`relayUDPConn` از `relayStream` استفاده نمی‌کند)؛ لاگ «stopped reading ... UDP/TUN backlog» همین را می‌گوید (`wedge.go:276-279`). روی خروجی، فشار حافظهٔ لبه دیده نمی‌شود (`peerMemPressure` فقط روی لبه پر می‌شود: `linkmanager.go:1667-1676`).
15. **مشاهده — L3 لینکِ گیرکرده می‌میرد.** `watchSession` با `rdCalls` کار می‌کند که وقتی `recvLoop` روی سطل خالی پارک است تکان نمی‌خورد (`l3_link.go:164-169`، `wedge.go:26-31`)؛ پس L3 لینکی که ۱۲s گیر کرده مرده اعلام و (طبق مشاهدهٔ ۱) دیگر باز نمی‌شود.
16. **مشاهده — لاگ‌های «link up from / link down from» خروجی direct تجمیع نمی‌شوند** (`stream_kharej.go:142`، `stream_kharej.go:154`)، برخلاف لاگ‌های لبه و خروجی reverse (`burstLog`).
17. **مشاهده — خروجی direct سقف پذیرش ندارد**؛ هر تعداد لینکی که دارندهٔ کلید مشترک بسازد پذیرفته می‌شود (سقف فقط در لبهٔ reverse است: `stream_reverse.go:68`). سقف خروجی در direct فقط گزارش می‌شود (`stream_kharej.go:48-52`).
18. **مشاهده — `writeDatagram` برای هر دیتاگرام یک تخصیص حافظه دارد** (`stream.go:250`) و دیتاگرام ورودی لبه هم کپی می‌شود (`stream_iran.go:409`).
19. **مشاهده — سرور TLS سمت `Handle` فقط `http/1.1` را در ALPN پیشنهاد می‌کند** (`tlscarrier/server.go:76-79`) در حالی که کلاینت اثر انگشت Chrome دارد؛ پیامد آن برای DPI نامطمئن است (موضوع زیرسیستم tlscarrier).
20. **مشاهده — `engine.go` (موتور بسته‌ای، `Engine`، `RunDial/RunListen`) در مسیر mtcp/l3mtcp به کار نمی‌رود**؛ فقط `sleepCtx` و `acceptBackoff` آن مشترک‌اند (`engine.go:200-239`). `carrier_tcp.go` (Noise روی TCP) هم مخصوص حامل‌های قدیمی است.

---

## ۱۴. ارجاع به زیرسیستم‌های دیگر

| از این زیرسیستم | به | چه چیزی |
|---|---|---|
| `RunIran` | `LinkManager` (`linkmanager.go`) | `NewLinkManager`، `SetDrainIdle`، `SetWarm`، `gateInfo`، `OnLink`، `Run`، `Stats`، `Pick`/`pickHeld`/`releaseFor`/`cancelHeld`، `exitInfo` |
| `acceptReverseLinks` | `LinkManager` | `alive`، `noteOverCap`، `AddLink`، `DropLink`، `max` |
| `openPoolCtl` | `LinkManager` (`poolCtlSource`) | `ctlTarget`، `targetChanged`، `poolCtlFast`، `poolCtlLive`، `markPoolRefused` |
| `openControl`/`runStats`/`openInfo` | `linkMeter` (`health.go`) | سیگنال‌هایی که `sampleHealth`، `stuck.go` و `loss.go` مصرف می‌کنند |
| `newSession` | `wedge.go` | `sessGuard`، `guards.add`؛ `relayStream`، `guardOf` |
| `newSession` | `obfs/shaper.go` | `NewHTTPSLengthSampler`، `Sample` |
| `mtcpDialer`، `serveStream`، `serveControl`، `serveStats` | `tlscarrier` | `DialFrom`، `DialFromTimeout`، `Server.Handle`، `Carrier.RawConn/TCPConn` |
| `serveStats`، `mtcpLink.tcpStats`، `serveControl` | `tcpStats`/`retransmits` (سوکت TCP_INFO، فایل‌های health/stats لینوکس) | |
| `serveStats` | `mempressure.go` | `tcpMemPressure` |
| `serveStream` | `routes.go` | `RouteTable.Target`، `noRouteLog` |
| `openL3`، `serveStream` | `l3_link.go` | `newStreamL3Link`، `l3Set` |
| `exitPool` | `dialgate.go` | `linkGate.acquireIf` |
| `exitPool`، `LinkManager` | `burstlog.go` | تجمیع لاگ |
| `listenReuseRcvBuf` | `dgforward.go`، `dgports.go` | (هم توسط dgtun استفاده می‌شود) |
| فراخوانندگان این زیرسیستم | `cmd/hs2/main.go:453-551` | `runStream` ⇒ `RunIran`/`RunKharej`، `NewMTCPDialer`، `ListenReuse`، `WarmSize`، `ParsePortMap`؛ `OnStart` ⇒ `startStatusWriter` (نمایش وضعیت)؛ `applyTuning` ⇒ `SmuxFrameSize`/`SmuxStreamBuffer`/`SmuxSessionBuffer`/`NotSentLowat` |
