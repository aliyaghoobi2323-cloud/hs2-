# حامل UDP، کنترل نرخ، FEC

> دامنه: `hs2-src/udpcarrier/*`، `hs2-src/fec/*`، `hs2-src/mmsg/*` و نقطهٔ اتصالشان به موتور (`engine/carrier_udp.go`، `engine/dgcarrier.go`، `engine/dgpool.go`، `cmd/hs2/main.go`).
> همهٔ مسیرها نسبت به `/home/user/hs2-/hs2-src/` است، مگر خلافش گفته شود (`../CHANGELOG.md` یعنی `/home/user/hs2-/CHANGELOG.md`).
> عددهایی که با برچسب «محاسبه‌شده» آمده‌اند را خودم با همان فرمول کد (`fec/adapt.go`) حساب کرده‌ام؛ در کد به این شکل نوشته نشده‌اند.
> هیچ فایلی در مخزن تغییر نکرد.

---

## ۱. نقش و جایگاه در کل سیستم

- **این زیرسیستم روی مسیر دادهٔ تونل اصلی کاربر (`l3mtcp`) نیست.** تنها جاهایی که بیرون از این سه بسته به آن‌ها ارجاع می‌دهند این‌هاست (جستجوی `grep` روی کل کد غیرآزمونی):
  - `engine/carrier_udp.go` (حامل‌های `udp` و `auto`)،
  - `engine/dgcarrier.go` و `engine/dgpool.go` (حامل `dgtun`، یعنی استخری از حامل‌های datagram زیر TUN)،
  - `cmd/hs2/status.go:330` (`udpcarrier.SetHostSaturated`)،
  - `encap/raw_linux.go` و `encap/rawtx_linux.go` (فقط بستهٔ `mmsg` برای سوکت‌های خام icmp/gre/ipip/ipx)،
  - `lab/dglab/main.go` (ابزار آزمایشگاه).
  - در `engine/stream*.go` و `engine/linkmanager.go` (مسیر `mtcp`/`l3mtcp`/`tls`) هیچ استفاده‌ای نیست؛ تنها اشارهٔ `linkmanager.go:2286` یک توضیح در ساختار مشترک آمار `PoolStats` است.
- حامل‌هایی که از این زیرسیستم استفاده می‌کنند (`cmd/hs2/main.go:405-424`):
  - `udp`: یک حامل Noise + FEC روی UDP، زیر موتور قدیمی تک‌حاملی (`runUDP`، `cmd/hs2/main.go:795`).
  - `auto`: اول کاوش UDP، در صورت مناسب نبودن، حامل TCP از نوع `noise` (نه TLS) (`engine/carrier_udp.go:57-90`).
  - `dgtun`: استخری خودتنظیم از همین حامل‌ها روی کپسوله‌سازی‌های udp/icmp/gre/ipip/ipx، با autopilot و governor (`cmd/hs2/main.go:829-904`، `engine/dgpool.go`).
- چرا وجود دارد (`udpcarrier/doc.go:7-16`، `fec/doc.go:5-12`): مسیر هدف ۲۶٪ به بالا اتلاف انفجاری دارد. TCP این اتلاف را ازدحام می‌خواند و پنجره را نصف می‌کند. این حامل بازارسال ندارد، اتلاف را ازدحام نمی‌داند، با parity (پهنای باند) بسته‌های گم‌شده را بازسازی می‌کند و نرخ ارسال را از مدل «پهنای باند/تأخیر» تنظیم می‌کند، نه از اتلاف. هدف اعلام‌شده «سرعت خام» نیست. هدف این است که تأخیر و jitter صاف بماند (`udpcarrier/REPORT.md:5-9`).
- نقش‌ها: سمت ایران همیشه edge و سمت خارج همیشه exit است. جهت (direct/reverse) فقط تعیین می‌کند چه کسی dial کند: `dialing = (mode=="dial") XOR reverse` (`cmd/hs2/main.go:430` و `:907`). در حالت direct، ایران کلاینت UDP است و خارج Listener.
- لایه‌ها، از بالا به پایین:
  `engine` (TUN، keepalive، اتصال مجدد) → `udpcarrier.Conn` (رمزنگاری datagram، FEC، pacer، کنترل نرخ، بازخورد) → `fec` (Reed-Solomon درهم‌چیده و تطبیقی) → `encap` (سوکت udp یا خام) → `mmsg` (sendmmsg/recvmmsg).

---

## ۲. اجزای اصلی

### ۲-۱. نوع‌ها

| نوع | محل | نقش |
|---|---|---|
| `Conn` | `udpcarrier/carrier.go:75-159` | یک حامل. رابط `Carrier` موتور (`SendFrame`/`ReadFrame`/`Close`) را به‌شکل ساختاری پیاده می‌کند. |
| `rateControl` | `udpcarrier/rate.go:112-215` | مدل نرخ (BBR-lite + کنترل صف یک‌طرفه + قواعد استخر). |
| `minFilter` | `udpcarrier/rate.go:402-426` | کمینهٔ پنجره‌ای دوسطلی، در عمل روی بازهٔ [w, 2w). |
| `bwSample` | `udpcarrier/rate.go:217-222` | نمونهٔ نرخ تحویل‌شده، همراه پرچم `limited`. |
| `pacer` | `udpcarrier/pacer.go:25-78` | سطل توکن، سه صف (parity، fast، data)، بودجهٔ زمانی صف، ارسال دسته‌ای. |
| `Governor` | `udpcarrier/governor.go:38-78` | هماهنگ‌کنندهٔ همهٔ حامل‌های یک استخر: کشف policer، سقف مشترک، سهم عادلانه، صف حامل‌های پرکار. |
| `Listener` / `peerLink` | `udpcarrier/listen.go:23-50` | سمت پاسخ‌دهنده: یک سوکت مشترک، demux بر اساس نشانی مبدأ. |
| `packetSocket` | `udpcarrier/socket.go:15-28` | انتزاع سوکت Listener (pktinfo و دسته‌ای). |
| `udpListenBatch` | `udpcarrier/batch_linux.go:87-99` | خواندن و نوشتن دسته‌ای روی سوکت UDP شنونده، فقط IPv4. |
| `EncapConfig` | `udpcarrier/dial.go:19-23` | نوع کپسوله‌سازی، IP مبدأ و شمارهٔ پروتکل ipx. |
| `feedback` | `udpcarrier/wire.go:59-67` | گزارش دوره‌ای گیرنده. |
| `ProbeResult` / `probeResponder` | `udpcarrier/probe.go:72-79` و `:201-213` | کاوش دسترسی و اتلاف. |
| `Stats` | `udpcarrier/carrier.go:720-746` | نمای وضعیت برای `hs2 status`، لاگ و آزمون. |
| `fec.Encoder` / `fec.Config` | `fec/encoder.go:12-34` و `:90-117` | تولید shard، درهم‌چینی، parity. |
| `fec.Decoder` | `fec/decoder.go:10-26` | بازسازی و حذف تکراری. |
| `fec.Adapter` | `fec/adapt.go:24-31` | تبدیل گزارش اتلاف به برآوردی برای اندازه‌گذاری parity. |
| `mmsg.Batch` | `mmsg/mmsg_linux.go:29-33` | آرایه‌های هسته برای sendmmsg/recvmmsg. |

### ۲-۲. goroutineها برای هر `Conn`

| goroutine | محل | کار |
|---|---|---|
| `processLoop` | `udpcarrier/carrier.go:409-448` | تنها کاربر Decoder و پنجرهٔ replay: جداسازی tag، `trackWire`، OWD، decode، expire، رمزگشایی. |
| `feedbackLoop` | `udpcarrier/carrier.go:537-583` | هر ۱۰۰ms یک گزارش (با camo لرزان؛ در بی‌کاری حدود ۰٫۷ ثانیه). |
| `flushLoop` | `udpcarrier/carrier.go:705-717` | هر ۱۰ms گروه‌های FEC قدیمی‌تر از `Window` را می‌بندد و parity آن‌ها را بیرون می‌دهد. |
| `pacer.loop` | `udpcarrier/pacer.go:252-441` | مالک همهٔ نوشتن‌های داده روی سوکت. |
| `readPump` (فقط سمت dial، روی UDP) | `udpcarrier/dial.go:135-176` | خواندن سوکت، دسته‌ای (recvmmsg) یا تکی. |
| `runServerConfirm` / `runClientConfirm` | `udpcarrier/dial.go:231-299` | تأیید کلید پس از دست‌دهی. |
| `Listener.serve` (یکی برای هر Listener) | `udpcarrier/listen.go:93-125` | یک حلقهٔ خواندن برای همهٔ حامل‌ها و کاوش‌ها. |
| `Governor.Run` (یکی برای هر استخر) | `udpcarrier/governor.go:144-155`؛ راه‌اندازی در `engine/dgpool.go:2216` و `:2253` | تیک ۵۰۰ms. |

### ۲-۳. توابع کلیدی

- ارسال: `SendFrame` (`carrier.go:231`)، `sendDataLane` (`:265-285`)، `sendControl` (`:289-304`)، `SendUrgent` (`:258`)، `pacer.enqueueLane` (`pacer.go:179-231`).
- دریافت: `feed`/`tryFeed` (`carrier.go:385-405`)، `processLoop`، `onPayload` (`:451-474`)، `ReadFrame` (`:309-326`)، `TryReadFrame` (`:331-338`).
- بازخورد: `sendFeedback` (`:585-609`)، `Conn.onFeedback` (`:486-533`)، `rateControl.onFeedback` (`rate.go:451-566`)، `adjustLocked` (`rate.go:575-818`)، `pacingRate` (`rate.go:861-879`).
- اتلاف سیم: `trackWire` (`carrier.go:639-669`)، `wireLossPPM` (`:618-633`).
- FEC: `Encoder.Encode` (`fec/encoder.go:207-246`)، `trackRate` (`:253-291`)، `Flush` (`:302-311`)، `closeLocked` (`:330-373`)، `Decoder.Decode` (`fec/decoder.go:110-182`)، `tryRecover` (`:186-250`)، `Adapter.Observe` (`fec/adapt.go:100-126`)، `parityForStep` (`:208-222`).
- Governor: `tick` (`governor.go:284-477`)، `reserve` (`:250-273`)، `lift` (`:480-493`).
- دست‌دهی: `DialCfg` (`dial.go:80-131`)، `dialHandshake` (`:180-223`)، `Listener.tryHandshake` (`listen.go:166-220`).
- انتخاب خودکار: `autoDialer.Dial` (`engine/carrier_udp.go:69-84`).

---

## ۳. جریان داده و کنترل، گام‌به‌گام

### ۳-۱. مسیر ارسال داده (TUN → سیم)

1. موتور `SendFrame(core.TypeData, ipPacket)` را صدا می‌زند (`carrier.go:231-236`). در استخر dgtun، بسته‌های یک جریان تعاملی با `SendUrgent` فرستاده می‌شوند (`engine/dgpool.go:419-449`).
2. `sendDataLane` (`carrier.go:265-285`) این کارها را انجام می‌دهد:
   - اگر pacer قبلاً خطای نوشتن داشته، همان خطا برمی‌گردد (`:271-273`)؛
   - قفل `sendMu` را می‌گیرد؛
   - `sess.AppendDatagram` قاب را در جا، به شکل `[seq:8][ciphertext]` و با padding دسته‌اندازهٔ B6، مهر و موم می‌کند (`:275`)؛
   - `enc.Encode` را صدا می‌زند و هر shard را به `pacer.enqueueLane` می‌دهد (`:282`).
   - قفل `sendMu` تا وقتی emit برگردد نگه داشته می‌شود. فشار برگشتی pacer همین‌جا به نویسنده منتقل می‌شود.
3. `Encoder.Encode` (`fec/encoder.go:207-246`):
   - قفل را می‌گیرد؛
   - `trackRate` را اجرا می‌کند (هر ۵۰ms عمق درهم‌چینی را از روی pps بازمحاسبه می‌کند)؛
   - payload را round-robin به یکی از `lanes` (گروه‌های باز) می‌دهد؛
   - shard دادهٔ `[hdr:9][len:2][payload]` را **فوراً** emit می‌کند؛
   - اگر گروه به K=8 رسیده باشد، آن را می‌بندد (`closeLocked`) و r تا shard توازن را emit می‌کند.
4. `pacer.enqueueLane` (`pacer.go:179-231`):
   - بسته را با ۹ بایت فضای سرآیند در یک بافر pool کپی می‌کند؛
   - parity به صف `pri` می‌رود، فوری به `fast`، باقی به `in`؛
   - برای دادهٔ عادی تا وقتی `queued > budget()` باشد صبر می‌کند (`:193-207`). `budget` برابر است با بیشینهٔ (نرخ × ۲۰ms) و ۴۵۰۰ بایت؛
   - زمان انتظار هم در `heldNs` و هم در `rc.noteStageHeld` ثبت می‌شود.
5. `pacer.loop` (`pacer.go:252-441`):
   - ترتیب برداشتن: اول `pri`، بعد `fast`، بعد `in`؛
   - توکن را از روی `pacingRate` شارژ می‌کند و سقف ترکیدن (`pacerBurstCap`) را اعمال می‌کند؛
   - اگر توکن کم باشد صبر می‌کند (هر انتظار حداکثر ۵۰ms)؛
   - اگر Governor سقف گذاشته باشد، `g.reserve(len)` را اجرا می‌کند؛
   - اگر سقف نباشد، تا ۱۶ datagram آماده را که توکن کافی دارند جمع می‌کند؛
   - در ترتیب سیم سرآیند را پر می‌کند: `tagDataTS` با `wireSeq` و `stamp`، یا `tagData` با `wireSeq`؛
   - با `write` یا `writeBatch` (sendmmsg) می‌فرستد؛
   - `rc.onSent(len)` را برای همهٔ بایت‌های سیم صدا می‌زند، یعنی داده + parity + سرآیند.
6. `flushLoop` هر ۱۰ms گروه‌های بازِ قدیمی‌تر از ۶۰ms را می‌بندد و parity آن‌ها را به `pacer.enqueue` می‌دهد (`carrier.go:705-717`).

### ۳-۲. مسیر دریافت (سیم → TUN)

1. سمت dial: `readPump` روی سوکت متصل اجرا می‌شود (دسته‌ای تا ۳۲ تا، `batch_linux.go:56-82`) و `c.feed` را صدا می‌زند، که مسدودکننده است. در کپسوله‌سازی خام، `SetReceiver` به `tryFeed` وصل است (`dial.go:115-122`).
   سمت listen: `serve` → `handle` → `pl.c.tryFeed(pkt)` (`listen.go:130-156`)، که مسدودکننده نیست. اگر صف `rx` (۱۰۲۴) پر باشد، بسته دور ریخته و `rxDropped` شمرده می‌شود (`carrier.go:398-405`).
2. زمان دریافت همان لحظهٔ بیرون آمدن از سوکت مهر می‌خورد (`rxPkt.at`، `carrier.go:385-390`)، تا تأخیر صف decode وارد اندازه‌گیری RTT و OWD نشود.
3. `processLoop` (`carrier.go:409-448`):
   - `lastRxMono` را به‌روز می‌کند؛
   - برای tag `0x00`/`0x02`: `trackWire(wireSeq)` و `wireBytes += len`؛ در شکل مهردار، `noteOWD(stampOf(now) - stamp)`؛ سپس `dec.Decode`؛ هر ≥۵۰ms یک `dec.Expire`؛ آمار decoder منتشر می‌شود؛
   - برای tag `0x01`: مستقیم `onPayload`.
4. `Decoder.Decode` (`fec/decoder.go:110-182`):
   - shard داده‌ای که قبلاً دیده نشده **فوراً** تحویل می‌شود؛
   - parity ذخیره می‌شود؛
   - وقتی تعداد shardهای موجود ≥ k شد، `ReconstructData` اجرا و shardهای گم‌شده تحویل می‌شوند (`:186-250`).
5. `onPayload` (`carrier.go:451-474`):
   - `unpackDatagram`، بعد `sess.OpenDatagram` (AEAD + پنجرهٔ replay)؛
   - بر اساس نوع قاب: `TypeFeedback` → `onFeedback`؛ `TypeAuth` → `confCh`؛ `TypeData` → `rxDataBytes` و `deliver`؛ Ping/Pong/Close/PoolCtl/LinkStats → `deliver`.
6. `deliver` قاب را در کانال `frames` (۲۰۴۸) می‌گذارد و بعد `ReadFrame`/`TryReadFrame` آن را برمی‌دارند.

### ۳-۳. حلقهٔ بازخورد (گیرنده → فرستنده)

1. گیرنده هر ۱۰۰ms `sendFeedback` را اجرا می‌کند (`carrier.go:585-609`). گزارش شامل این‌هاست:
   - `sendNanos` (ساعت دیواری گیرنده)؛
   - `echoNanos` و `echoDelayNanos` (پژواک آخرین گزارشی که از طرف مقابل گرفته و مدت نگه داشتنش)؛
   - `rxDataBytes = wireBytes`: بایت‌های همهٔ datagramهای داده‌ای که رسیده، **پیش از FEC** و شامل parity (`:598-601`)؛
   - `lossPPM` از `wireLossPPM`؛
   - `owdTicks` (کمینهٔ OWD در این بازه) و پرچم‌های `fbStamps|fbOWD`.

   گزارش به‌شکل یک قاب کنترلی مهروموم‌شده و **بدون FEC و بدون pacer** فرستاده می‌شود.
2. فرستنده در `Conn.onFeedback` (`carrier.go:486-533`):
   - گزارشی که `sendNanos` آن ≤ آخرین مقدار دیده‌شده باشد را دور می‌ریزد (`:493-496`)؛
   - RTT را حساب می‌کند: `now − echoNanos − echoDelayNanos` (`:498-504`)؛
   - با دیدن `fbStamps`، ارسال مهردار را روشن می‌کند (`:505-507`)؛
   - `Share`، `Mean`، `BusyQueue` و `Capped` را از Governor می‌گیرد (`:508-510`)؛
   - `rc.onFeedback` را اجرا می‌کند (`:511`)؛
   - `g.report(c, loss, queue)` (`:513-515`)؛
   - اگر policer تأییدشده باشد، اتلاف ورودی FEC را به `CleanLoss+0.01` محدود می‌کند (`:521-523`)؛
   - `adapter.Observe` → `enc.SetLoss` (`:524-526`)؛
   - این گزارش را برای پژواک در گزارش بعدی خودش ذخیره می‌کند (`:529-532`).

### ۳-۴. دست‌دهی و تأیید کلید

1. کلیدهای ایستا از رمز مشترک ساخته می‌شوند: `StaticFromSeed(shared, "hs2-udp-responder"/"hs2-udp-initiator")` (`dial.go:45-52`، `core/datagram.go:75-84`). psk هم همان `shared` است.
2. Dialer:
   - `encap.Dial` (`dial.go:88`)؛
   - `dialHandshake`: پیام ۱ تا ۱۲ بار، با انتظار `300+150×attempt` میلی‌ثانیه، و حداکثر در ۱۰ ثانیه یا تا مهلت ctx (`:189-219`). پاسخ نامعتبر نادیده گرفته می‌شود و تلاش ادامه پیدا می‌کند (`:207-212`)؛
   - سپس `newConn`، وصل کردن batch، و `readPump`.
3. Listener:
   - datagram از نشانی ناشناخته: اول به‌عنوان کاوش بررسی می‌شود (`probe.handle`)؛ اگر کاوش نبود، `tryHandshake` (`listen.go:149-156`)؛
   - `ReadMessage1Payload` پیش از هر کار Noise یک MAC ارزان کلیددار با psk و سطل زمانی ۳۰ثانیه‌ای را چک می‌کند و یک مجموعهٔ replay هم دارد (`core/handshake.go:186-211`، سطل `:74`، پنجره ۹۰ ثانیه `:33`)؛
   - کلید ایستای initiator را pin می‌کند (`listen.go:174`)؛
   - پیام ۲ را می‌فرستد، `peerLink{m1,m2}` را cache می‌کند، و پیام ۱ تکراری را با همان پیام ۲ cacheشده جواب می‌دهد (`:142-144`).
4. تأیید (`auth.go`):
   - `HMAC-BLAKE2s(shared, domain‖binding‖Exporter("hs2-udp-channel-binding",32))` بریده به ۱۶ بایت، با دامنهٔ `hs2-udp-cli-v1` یا `hs2-udp-srv-v1` (`auth.go:30-53`)؛
   - کلاینت هر ۱۵۰ms بازارسال می‌کند و تا ۶ ثانیه صبر می‌کند (`dial.go:126`، `:227`)؛
   - سرور با مهلت ۳ ثانیه منتظر می‌ماند، به هر تگ معتبر جواب می‌دهد، با اولین تگ معتبر حامل را تحویل `Accept` می‌دهد (کانال ۸ تایی، `listen.go:82`) و ۳ ثانیهٔ دیگر هم به بازارسال‌ها جواب می‌دهد (`dial.go:266-299`، `listen.go:213-219`).

### ۳-۵. انتخاب خودکار در `auto` (UDP یا TCP)

`engine/carrier_udp.go:69-84`:

1. `ProbeFrom(addr, bindIP, shared, n=16, interval=8ms, timeout=300ms)` روی یک سوکت UDP **جداگانه** اجرا می‌شود (`probe.go:89-185`).
2. شرط `res.Reachable && res.Loss <= 0.45` (`maxRecoverableLoss`، `carrier_udp.go:24`) برقرار باشد ← `udpcarrier.DialFrom`. اگر dial شکست بخورد ← TCP.
3. در غیر این صورت ← `tcpFallback`: یک `noiseDialer` روی **همان پورت به‌شکل TCP**، با همان کلیدهای مشتق از رمز (`:86-90`).
4. سمت شنونده، `autoListener` هر دو را روی یک نشانی سرویس می‌دهد و هر کدام زودتر برقرار شد را تحویل می‌دهد (`:114-171`).
5. تصمیم فقط **هنگام dial** گرفته می‌شود. بازگشت به UDP فقط وقتی ممکن است که حامل TCP بمیرد و موتور دوباره dial کند (`:14-19`). موتور: keepalive هر ۵ ثانیه، مرگ بعد از ۱۵ ثانیه، backoff از ۰٫۵ تا ۸ ثانیه (`engine/engine.go:29-30`، `:66-79`).

---

## ۴. جدول ثابت‌ها، آستانه‌ها، بافرها و زمان‌سنج‌ها

### ۴-۱. حامل (`udpcarrier/carrier.go` و دیگر فایل‌های همین بسته)

| نام | مقدار | محل | معنی |
|---|---|---|---|
| `deadAfter` | 15s | carrier.go:54 | اگر این مدت هیچ چیزی نرسد، `ReadFrame` خطای `errDeadLink` می‌دهد. |
| `feedbackEvery` | 100ms | carrier.go:55 | دورهٔ گزارش بازخورد. |
| `flushEvery` | 10ms | carrier.go:56 | تیک بستن گروه‌های FEC. |
| `expireEvery` | 50ms | carrier.go:57 | فاصلهٔ Expire در decoder (فقط وقتی داده برسد). |
| `camoIdlePoll` | 700ms | carrier.go:41 | فاصلهٔ بازخورد در بی‌کاری وقتی camo روشن است (±۲۰٪). |
| لرزش بازخورد camo | ±40% | carrier.go:557, 576 | `camoJitter(feedbackEvery, 0.4)`. |
| FEC `K` | 8 | carrier.go:167 | تعداد shard داده در هر گروه. |
| FEC `Window` | 60ms | carrier.go:167 | بازهٔ پخش گروه؛ گروه باز پس از این مدت بسته می‌شود. |
| FEC `MaxDepth` | 64 | carrier.go:167 | بیشینهٔ تعداد گروه‌های درهم‌چیده. |
| FEC `MaxPayload` | innerMTU+64 | carrier.go:162 | (۱۳۴۴ برای MTU 1280). |
| TTL decoder | 220ms | carrier.go:187 | مدت انتظار گروه ناقص برای parity. |
| کانال `rx` | 1024 | carrier.go:180 | datagramهای منتظر decode. |
| کانال `frames` | 2048 | carrier.go:181 | قاب‌های منتظر `ReadFrame`. |
| کانال `confCh` | 4 | carrier.go:182 | تگ‌های تأیید. |
| عمق صف‌های pacer | 64 (کمینه) | carrier.go:190؛ pacer.go:151-153 | ظرفیت هر یک از کانال‌های `in`، `pri` و `fast`. |
| `wireConfirmWin` | 64 | carrier.go:224 | چند seq بالاتر باید برسد تا یک seq غایب «گم‌شده» حساب شود. |
| `wireReorderWin` | 256 | carrier.go:225 | اندازهٔ بیت‌مپ ورود. |
| `DefaultInnerMTU` | 1280 | dial.go:41 | MTU پیش‌فرض تونل. |
| `CarrierOverhead` | 56 | mtu.go:11 | سربار ثابت: ۹ + ۱۱ + ۸ + ۲۸. |
| `PathMTU` | 1500 | mtu.go:14 | |
| `InnerMTUFor` | udp/gre 1416، icmp 1408، ipip/ipx 1420 | mtu.go:27-32؛ mtu_test.go:117 | بیشینهٔ MTU بدون قطعه‌قطعه شدن. |
| تلاش‌های دست‌دهی | 12، انتظار 300+150·i ms، سقف 10s | dial.go:190-198 | |
| `confirmResend` | 150ms | dial.go:227 | |
| مهلت تأیید کلاینت | 6s | dial.go:126 | |
| مهلت تأیید سرور | 3s (و ۳ ثانیه پس از پذیرش) | listen.go:213؛ dial.go:295 | |
| کانال accept | 8 | listen.go:82 | |
| طول تگ تأیید | 16 | auth.go:30 | |
| بستهٔ کاوش | 40 بایت | probe.go:33-39 | magic ۴ + nonce ۸ + seq ۴ + زمان ۸ + تگ ۱۶. |
| پیش‌فرض کاوش | n=20، interval=10ms، timeout=500ms | probe.go:90-98 | |
| کاوش در `auto` | n=16، interval=8ms، timeout=300ms | engine/carrier_udp.go:70 | |
| `maxRecoverableLoss` | 0.45 | engine/carrier_udp.go:24 | آستانهٔ اتلاف کاوش برای انتخاب UDP. |
| `stampTick` | 125µs | wire.go:117 | واحد مهر زمان ارسال؛ هر حدود ۶ روز یک بار سرریز ۳۲بیتی. |
| `feedbackLen` / `feedbackLenExt` | 36 / 41 | wire.go:52-53 | |
| `udpBatch` | 32 | batch_linux.go:28 | بیشینهٔ datagram در هر syscall روی سوکت UDP. |
| بافر خواندن | 2048 بایت | batch_linux.go:67, 111؛ dial.go:157؛ listen.go:111 | datagram بریده‌شده دور ریخته می‌شود. |
| `pktinfoOOB` | 64 | pktinfo_linux.go:23 | |

### ۴-۲. pacer (`udpcarrier/pacer.go`)

| نام | مقدار | محل | معنی |
|---|---|---|---|
| `pacerQueueTime` | 20ms | :82 | بودجهٔ زمانی صف داده: نرخ × ۲۰ms. |
| `pacerQueueMin` | 4500B | :83 | کف بودجه؛ در نرخ کف هنوز حدود ۱۴۰ms است. |
| `pacerBatch` | 16 | :87 | بیشینهٔ datagram در هر ارسال دسته‌ای. |
| `pacerQuantum` | 2ms | :92 | سقف عادی سطل توکن: نرخ × ۲ms، و دست‌کم دو datagram. |
| `pacerLateCredit` | 10ms | :104 | سقف سطل برای حاملی که مرحلهٔ ارسالش آن را عقب نگه داشته و داده در صف دارد. |
| `pacerSatCredit` | 50ms | :120 | همان، وقتی میزبان اشباع است (`hostSaturated`). |
| سقف یک انتظار | 50ms | :316 | |
| سرآیند رزروشده | 9 بایت | :184 | شکل بی‌مهر از ۵ بایت آخر استفاده می‌کند. |
| بافر pool | ظرفیت 2048 | :167 | |

### ۴-۳. کنترل نرخ (`udpcarrier/rate.go`)

| نام | مقدار | محل | معنی |
|---|---|---|---|
| نرخ اولیه | 125000 B/s (حدود ۱ Mbit/s) | :432 | |
| `srtt` / `rtProp` اولیه | 50ms | :430-431 | |
| `minRate` | 32000 B/s (۲۵۶ kbit/s) | :438 | کف نرخ. |
| `maxRate` | 4e9 B/s | :439 | سقف. |
| `bwWindowRounds` | 8 | :225 | پنجرهٔ بیشینهٔ تحویل؛ «دور» فقط روی نمونهٔ limited یا رکورد جلو می‌رود. |
| `owdWindow` | 10s | :226 | پنجرهٔ کمینهٔ OWD و RTT؛ هر سطل ۵ ثانیه (`:436-437`). |
| `lowQueue` | 5ms | :231 | زیر این، «صفی نایستاده است». |
| `targetQueue` | 10ms | :232 | هدف صف. |
| `queueTau` | 250ms | :233 | ثابت زمانی؛ مقدار واقعی τ = max(250ms, 2.5×srtt) (`:780`). |
| `startupPlateau` | 3 | :235 | سه بار رشد کمتر از ۲۵٪ در هر RTT، یعنی پایان startup. |
| `startCap` | 100 | :236 | پشتیبان پایان startup، حدود ۱۰ ثانیه گزارش limited. |
| `startupGain` | 2.885 | :237 | ضریب BBR برابر 2/ln2. |
| `deliveryCap` | 1.1 | :238 | سقف زیر policer: ۱٫۱ × میانگین تحویل. |
| `looseCap` | 2.5 | :239 | سقف عادی: ۲٫۵ × btlBw. |
| `rateLossExcess` | 0.05 | :240 | اتلاف در نرخ کامل که ۵٪ بیشتر از اتلاف هنگام probe باشد، نشانهٔ policer است. |
| `fullLossAlpha` | 0.1 | :241 | |
| `noQueueFor` | 2s | :242 | شرط policer: این مدت بدون صف. |
| `capAlpha` | 0.25 | :243 | EWMA ظرفیت هنگام وجود صف. |
| `capFloorFrac` | 0.4 | :244 | کف ظرفیت: ۰٫۴ × btlBw (فقط وقتی limited است یا قواعد خاموش است). |
| `minDrainGain` | 0.5 | :245 | |
| `maxQueueGain` | 1.1 | :246 | (در عمل دست‌نیافتنی؛ بخش ۱۳). |
| `probeGrow` | 1.04 | :247 | رشد ظرفیت در هر گزارش وقتی صف نیست (در RTT بالای ۱۰۰ms، در هر RTT). |
| `baseProbeEvery` | 4s | :257 | |
| `baseProbeDur` | 300ms | :258 | مدت واقعی = max(300ms, srtt+100ms) با سقف ۱ ثانیه (`:791-792`). |
| `maxProbeDur` | 1s | :259 | |
| `baseProbeGain` | 0.75 | :260 | نرخ هنگام probe: ۰٫۷۵ × min(rate, btlBw) (`:872-876`). |
| `limitedShare` | 0.8 | :261 | اگر ≥۸۰٪ اجازه فرستاده شده باشد، limited یا «pushing» است. |
| `maxLossComp` | 0.5 | :262 | جبران اتلاف هرگز بیش از ۵۰٪ فرض نمی‌کند. |
| `baseLossAlpha` | 0.02 | :263 | میانگین بلندمدت اتلاف (حدود ۵ ثانیه). |
| `probeLossAlpha` | 0.5 | :264 | |
| `overflowQueue` | 30ms | :265 | |
| `minRTTScale` | 30ms | :266 | کف مقیاس رشد برای هر RTT. |
| `rttSaneMax` | 30s | :267 | |
| `startupQueueRuns` | 3 | :270 | سه گزارش با صف، پایان startup (قاعدهٔ استخر). |
| `startupQueueMin` | 128000 B/s | :271 | حدود ۱ Mbit/s. |
| `startupDeepRuns` / `startupDeepMax` | 10 / 30 | :272-273 | گزارش‌های صف عمیق برای حامل سبک. |
| `deepShortfall` | 0.95 | :274 | |
| `fairStep` / `fairPull` / `fairOwnMax` | 0.005 / 0.10 / 0.03 | :275-277 | رشد به سمت سهم عادلانه. |
| `bwStaleAge` | 1.5s | :278 | |
| `stageHeadroom` | 2.0 | :289 | |
| `stageRegrow` | 1.25 | :290 | |
| `sendAvgAlpha` | 0.25 | :291 | |
| `stageHold` | 2s | :292 | |
| `stageForget` | 10s | :296 | |
| `stageHeldMin` | 0.25 | :300 | |
| `fairQueueMax` | 20ms | :304 | |
| `camoProbeJit` | ±1.5s | :369 | لرزش قطعی زمان probe وقتی camo روشن است. |
| ضریب EWMA برای `srtt` | 0.25 | :474 | |
| نگهبان dt گزارش | <20ms رد می‌شود | :509 | |

### ۴-۴. Governor (`udpcarrier/governor.go:108-133`)

| نام | مقدار | معنی |
|---|---|---|
| `govTickEvery` | 500ms | |
| `govShareHold` | 2 تیک | ماندن Share و BusyQueue بدون حامل پرکار. |
| `govHist` | 60 تیک (۳۰ ثانیه) | |
| `govBurstLoss` | 0.05 | کف اتلاف یک تیک episode. |
| `govBurstOverMed` / `govBurstOverAdd` | 3.0 / 0.03 | episode یعنی اتلاف ≥ ۳×میانه + ۰٫۰۳. |
| `govMinHist` | 10 | |
| `govCarrierLossy` | 0.02 | |
| `govSimultaneous` | 0.6 | دست‌کم ۶۰٪ حامل‌های pushing همزمان lossy باشند. |
| `govActiveRate` | 20000 B/s | |
| `govDetectWindow` | 30s | |
| `govCapFrac` | 0.9 | |
| `govLowerFrac` / `govTestLowerFrac` | 0.8 / 0.7 | |
| `govLiftBelow` | 0.45 | |
| `govRestMax` / `govRest` | 1h / 5min | |
| `govRaiseEvery` / `govRaiseGain` | 4s / 1.15 | |
| `govBindingFrac` | 0.85 | |
| `govFloorFrac` / `govFloorEpisodes` | 0.3 / 2 | |
| `govMinCap` | 125000 B/s | |
| `govBucket` | 5ms | ترکیدن سطل مشترک. |

### ۴-۵. FEC (`fec/*`)

| نام | مقدار | محل | معنی |
|---|---|---|---|
| `HeaderLen` | 9 | codec.go:12 | `[group:4][index:1][k:1][r:1][size:2]`. |
| `lenPrefix` | 2 | codec.go:15 | |
| `MaxShards` | 255 | codec.go:18 | k+r. |
| پیش‌فرض `K` | 32 | encoder.go:49 | حامل از آن **استفاده نمی‌کند** (K=8 است). |
| پیش‌فرض `Window` | 30ms | encoder.go:50 | حامل ۶۰ms می‌گذارد. |
| پیش‌فرض `MaxDepth` | 32 | encoder.go:51 | حامل ۶۴ می‌گذارد. |
| پیش‌فرض `MaxPayload` | 1400 | encoder.go:52 | |
| `TargetResidual` | 0.01 | encoder.go:53 | حامل همین را به ارث می‌برد. |
| `CeilRatio` | 1.5 | encoder.go:54 | حامل همین را به ارث می‌برد؛ برای K=8 بیشینهٔ r برابر ۱۲ است. |
| سقف K | 128 | encoder.go:78-80 | |
| `rateEvery` | 50ms | encoder.go:249 | |
| EWMA نرخ ورودی | 0.3 | encoder.go:267 | |
| عمق | round(pps × Window / K) در بازهٔ [1, MaxDepth] | encoder.go:273-279 | برای حامل: pps/133؛ در حدود ۸۵۰۰ pps به ۶۴ می‌رسد (محاسبه‌شده). |
| `maxGroups` decoder | 4096 | decoder.go:97 | پس از آن قدیمی‌ترین گروه بیرون رانده می‌شود. |
| free list بافر decoder | 2048 | decoder.go:100 | |
| spare گروه | 256 | decoder.go:287 | |
| TTL پیش‌فرض decoder | 500ms | decoder.go:92-94 | حامل ۲۲۰ms می‌گذارد. |
| `lossStep` | گام 0.5٪ و سقف 0.9 | adapt.go:198-206 | |
| Adapter `RiseAlpha` | 0.5 | adapt.go:58 | |
| `FallAlpha` | 0.08 | adapt.go:59 | ثابت زمانی حدود ۱٫۲ ثانیه. |
| `Hold` / `HoldFrac` | 2s / 0.5 | adapt.go:60-61 | |
| `Floor` | 0.03 | adapt.go:62 | |
| `Margin` | 0.02 | adapt.go:63 | |
| `Max` | 0.5 | adapt.go:64 | |

**جدول r برای K=8 در حامل** (محاسبه‌شده با `parityForStep` و `maxR=ceil(8×1.5)=12`؛ «برآورد» یعنی خروجی Adapter پس از Margin و Floor):

| برآورد اتلاف ≥ | r | سربار r/k |
|---|---|---|
| 0 (کف 0.03) | 1 | 12.5% |
| 0.035 | 2 | 25% |
| 0.070 | 3 | 37.5% |
| 0.115 | 4 | 50% |
| 0.150 | 5 | 62.5% |
| 0.185 | 6 | 75% |
| 0.220 | 7 | 87.5% |
| 0.255 | 8 | 100% |
| 0.280 | 9 | 112.5% |
| 0.315 | 10 | 125% |
| 0.340 | 11 | 137.5% |
| 0.365 | 12 (سقف) | 150% |

برای گروه‌های کوچکی که پنجرهٔ زمانی می‌بندد (k=1..4)، کمینهٔ r=1 است، یعنی k=1 سربار ۱۰۰٪ دارد (محاسبه‌شده).

### ۴-۶. mmsg

| نام | محل | معنی |
|---|---|---|
| `Supported` | mmsg_linux.go:18؛ mmsg_other.go | `true` فقط روی لینوکس. |
| اندازهٔ `Batch` | `NewBatch(n)`؛ mmsg_linux.go:36 | حامل UDP عدد ۳۲ را می‌سازد. |
| `EAGAIN` | mmsg_linux.go:68-70 و :104-114 | `Send` منتظر نوشتنی شدن سوکت می‌ماند. `SendNoLock` اول بدون قفل امتحان می‌کند و در `EAGAIN` به `rc.Write` برمی‌گردد. |
| `k == 0` | mmsg_linux.go:82-84 | خطای `"mmsg: sendmmsg sent nothing"`. |

---

## ۵. حلقه‌های کنترلی

### ۵-۱. کنترل نرخ: ورودی، شرط، خروجی

- **ورودی** (هر گزارش، حدود ۱۰۰ms): `rxDataBytes` (بایت سیمی که طرف مقابل دریافت کرده)، نمونهٔ RTT، `lossPPM`، `echoNanos`، `owdTicks` و `haveOWD`، به‌علاوهٔ `sent` (شمارندهٔ pacer)، `stageDrops`، `stageHeld`، `share`، `poolMean`، `busyQueue` و `govCapped`.
- **اندازه‌گیری RTT** (`rate.go:460-481`):
  - نمونه فقط وقتی پذیرفته می‌شود که تازه باشد (`echo` ≠ `lastEcho`)، کمتر از ۳۰ ثانیه باشد، و یکی از این سه برقرار باشد: نمونهٔ اول است، ≤ ۸×srtt است، یا سومین نمونهٔ پرت پشت سر هم است؛
  - `rtProp` کمینهٔ پنجره‌ای است و `srtt` یک EWMA با ضریب ۰٫۲۵.
- **صف** (`rate.go:483-500`):
  - اگر طرف مقابل مهر بزند: `q = OWD − min(OWD)` روی پنجرهٔ ۵ تا ۱۰ ثانیه (OWD یک‌طرفهٔ مسیر رفت، با اختلاف ساعت ثابت دو طرف)؛
  - وگرنه حالت پشتیبان: `q = min(دو نمونهٔ تازهٔ آخر RTT − rtProp)`.
- **نمونهٔ پهنای باند** (`rate.go:506-545`):
  - `dRate = Δrx/dt` و `sendRate = Δsent/dt`؛
  - `limited = sendRate ≥ 0.8×rate`؛
  - پنجره فقط وقتی جلو می‌رود که limited باشد یا `dRate > btlBw`؛
  - هنگام وجود صف، نمونه‌های قدیمی‌تر از ۱٫۵ ثانیه حذف می‌شوند (`:551-562`).
- **جبران اتلاف** (`rate.go:590-623`):
  - `baseLoss`: EWMA با ضریب ۰٫۰۲، بیرون از گزارش‌هایی که q ≥ ۳۰ms دارند؛
  - `probeLoss`: اتلاف درون base probe؛
  - `fullLoss`: اتلاف بیرون از probe؛
  - `policed` وقتی برقرار است که `fullLoss > probeLoss + 0.05` و ۲ ثانیه بدون صف گذشته باشد؛
  - `comp = 1/(1 − min(randomLoss, 0.5))` و `dComp = dRate × comp`.
- **startup** (`rate.go:626-718`):
  - در هر گزارش limited: `rate = max(2.885 × btlBw × comp, rate × 1.25^min(dt/srtt, 1))`؛
  - گزارشی که limited نیست چیزی را عوض نمی‌کند؛
  - **شرط‌های خروج** (هر کدام کافی است):
    1. limited باشد و q > ۵ms؛
    2. `queueFull`: قواعد روشن، ≥۳ گزارش متوالی با صف، `dRate ≥ 128KB/s`، و حامل «سبک» نباشد یا صف عمیق از آنِ خودش باشد؛
    3. plateau: btlBw سه بار در هر RTT کمتر از ۲۵٪ رشد کند و `dRate > 4×minRate`؛
    4. `startRounds ≥ 100`.
  - **مقدار `capEst` هنگام خروج** (`:696-710`):
    - اگر limited بود: max(dComp, btlBw×comp)؛
    - اگر با صف خارج شد: max(dComp, 0.4×btlBw×comp)؛
    - اگر به‌خاطر مرحلهٔ ارسال (stage) بود: 2×max(dComp, sendAvg)؛
    - اگر app-limited بود: max(capEst, rate).
- **پس از startup** (`rate.go:725-783`):
  - صف هست (q ≥ ۵ms): `capEst += 0.25(dComp − capEst)`؛ اگر limited است کف `0.4×btlBw×comp` اعمال می‌شود؛ اگر limited است و q < ۲۰ms، `fairGrow` اجرا می‌شود.
  - صف نیست و limited است: از گزارش دوم به بعد، `capEst *= 1.04^min(scale,1)`؛ یا اگر `stageRate>0` باشد، ضریب ۱٫۲۵.
  - صف نیست و حامل بی‌کار است: نرخ نگه داشته می‌شود.
  - **عبارت صف**: `rate = capEst × clamp(1 + (0.010 − q)/τ, 0.5, 1.1)`. با τ=250ms مقدار در q=0 برابر ۱٫۰۴، در q=10ms برابر ۱، و در q≥135ms برابر ۰٫۵ است.
- **base probe** (`rate.go:784-796`، `:861-877`):
  - روی ساعت مشترک ۴ثانیه‌ای استخر (`nextProbe`، `:345-365`)، فقط وقتی گزارش limited باشد؛
  - در مدت probe نرخ برابر `0.75×min(rate, btlBw)` است؛
  - هزینه حدود ۲٪ (`:256`).
- **سقف‌ها** (`rate.go:809-816`): فقط وقتی نمونهٔ limited در پنجره هست، `rate` و `capEst` به `2.5×btlBw×comp` محدود می‌شوند؛ اگر policed باشد، به `1.1×mean(delivery)×comp`. سپس clamp به [minRate, maxRate] (`:845-858`).
- **خروجی**: `pacingRate(now)` (`:861-879`) که pacer و بودجهٔ صف آن را می‌خوانند.
- **دوره**: یک بار در هر گزارش بازخورد (حدود ۱۰۰ms). همهٔ گام‌ها با `scale = dt / max(srtt, 30ms)` به «سهم یک RTT» مقیاس می‌شوند.

### ۵-۲. قواعد استخر (`fair`، با `HS2_FAIR_SHARE=0` خاموش می‌شود؛ `rate.go:62-111`)

1. startup با صفِ پایدار تمام می‌شود، حتی وقتی حامل از اجازه‌اش استفاده نمی‌کند (`queueFull`). استثنا: حامل «سبک»، یعنی با `lastSendRate < ref/2` که `ref` سهم استخر یا میانگین آن است (`light`، `:323-332`).
2. رشد به سمت سهم عادلانه فقط وقتی که صفِ ۵ تا ۲۰ms ایستاده باشد (`fairGrow`، `:388-396`).
3. ساعت مشترک probe، با فاصلهٔ دست‌کم ۳/۴ دوره.
4. قاعدهٔ مرحلهٔ ارسال (stage): حاملی که CPU یا سوکت عقب نگهش داشته، از startup بیرون می‌آید و `capEst ≤ 2×` آنچه واقعاً بیرون رفته (`:529-535`، `:583-589`، `:725-731`).

### ۵-۳. pacer

- **ورودی**: `pacingRate`، `stageLimited()`، `hostSaturated` و بودجهٔ Governor.
- **سطل توکن** (`pacer.go:299-337`):
  - شارژ: `tokens += Δt×rate`؛
  - سقف: `pacerBurstCap` (`:138-148`). برابر max(2×(len+64), rate×2ms) است؛ اگر حامل stage-limited باشد و داده در صف داشته باشد، rate×10ms؛ و اگر میزبان هم اشباع باشد، rate×50ms؛
  - پس از بیدار شدن تایمر هم دوباره سقف اعمال می‌شود (`:334`).
- **بودجهٔ صف داده** (`:235-241`): `max(rate×20ms, 4500B)`. parity و fast هرگز منتظر بودجه نمی‌مانند. نویسنده‌ای که صبر کند در `noteStageHeld` ثبت می‌شود.
- **ارسال دسته‌ای** (`:363-391`): تا ۱۶ datagram، با همان ترتیب صف‌ها، فقط تا جایی که توکن کافی باشد. زیر سقف Governor ارسال دسته‌ای انجام نمی‌شود.

### ۵-۴. Governor (هر ۵۰۰ms؛ `governor.go:284-477`)

- **ورودی**: Δsent هر حامل، میانگین اتلاف و صف گزارش‌شدهٔ هر حامل، و پرچم `pushing`.
- **خروجی‌های همیشگی**:
  - `meanB`: EWMA با ضریب ۰٫۵ از میانگین نرخ حامل‌های فعال؛
  - `shareB`: EWMA با ضریب ۰٫۵ از میانگین نرخ حامل‌های pushing، به شرط ≥۲ حامل؛
  - `busyQ`: میانهٔ صف حامل‌های pushing؛
  - `cleanLoss`: EWMA با ضریب ۰٫۱ از اتلاف تیک‌های غیر episode.
- **episode** (`:374-377`): ≥۲ حامل pushing، تاریخچهٔ ≥۱۰ تیک، اتلاف ≥ ۵٪ و ≥ ۳×میانه + ۳٪، صف < ۵ms، و ≥۶۰٪ حامل‌های pushing با اتلاف ≥ ۲٪.
- **حالت‌ها**: بخش ۶-۲.
- **بودجهٔ مشترک** (`reserve`، `:250-273`): سطل توکنی با نرخ `capB` و ترکیدن ۵ms. همهٔ datagramها، داده و parity، از آن برداشت می‌کنند.

### ۵-۵. FEC تطبیقی

- **Adapter** (`fec/adapt.go:100-159`). ورودی هر گزارش `loss = lossPPM/1e6` است:
  - EWMA نامتقارن: ۰٫۵ برای افزایش و ۰٫۰۸ برای کاهش؛
  - حفظ اوج: برآورد دست‌کم ۰٫۵ × بیشترین گزارش در ۲ ثانیهٔ اخیر است؛
  - سپس `+0.02` و clamp به [0.03, 0.5].
- خروجی Adapter با `enc.SetLoss` ثبت می‌شود و `parityFor(k)` را تعیین می‌کند. `parityFor` با کلید (k، گام ۰٫۵٪) cache می‌شود (`fec/encoder.go:186-203`): کوچک‌ترین r که `Residual(k,r,p) ≤ 1%` باشد، با سقف `ceil(1.5k)` و کف ۱.
- **عمق درهم‌چینی** (`encoder.go:253-291`): هر ۵۰ms از EWMA نرخ بسته‌ها (pps) دوباره حساب می‌شود. اگر عمق کم شود، گروه‌های اضافه فوراً بسته می‌شوند.
- **بستن گروه**: وقتی k به ۸ برسد، یا ۶۰ms گذشته باشد (`flushLoop`، تیک ۱۰ms)، یا عمق کم شود.
- **policer تأییدشده**: ورودی Adapter به `CleanLoss+1%` محدود می‌شود (`carrier.go:521-523`).

### ۵-۶. برآوردگر اتلاف سیم (گیرنده)

- `trackWire` (`carrier.go:639-669`) پنجرهٔ `[wireBase, wireBase+64)` را روی `wireSeq` جلو می‌برد. هر seq فقط یک بار «دریافت‌شده» یا «گم‌شده» ثبت می‌شود. جابه‌جایی ترتیب درون ۶۴ اتلاف حساب نمی‌شود.
- `wireLossPPM` نسبت Δlost/(Δrecv+Δlost) از گزارش قبل است (`:618-633`).
- این مقدار، ورودی هم کنترل نرخ است، هم Governor، هم Adapter.

---

## ۶. حالت‌ها و گذارها، خطاها و بازیابی

### ۶-۱. `rateControl`

- `startup=true` (`rate.go:433`)؛ با شرط‌های بخش ۵-۱ به `false` می‌رود و **هرگز به startup برنمی‌گردد**.
- پس از startup، زیرحالت‌های ضمنی:
  - «صف ایستاده»: `q≥lowQueue`؛
  - «probe ظرفیت»: بدون صف و limited؛
  - «نگه‌داشتن»: بدون صف و بی‌کار؛
  - «base probe»: `now<probeEnd`؛
  - «policed»؛
  - «stage»: `stageRate>0`، با فراموشی پس از ۱۰ ثانیه (`:587-588`)؛ پرچم `stageOn` تا ۲ ثانیه می‌ماند و بدون بازخورد خاموش می‌شود (`:923-932`).
- NaN یا مقدار منفی ← کف (`clampRateLocked`، `:845-858`).

### ۶-۲. Governor (`governor.go:402-476`)

- `govNormal` → `govCapped`:
  - شرط: یک episode تازه، دست‌کم ۲ episode در ۳۰ ثانیه، و تمام شدن دورهٔ استراحت؛
  - سقف = `0.9 × passedRate(30s)` (میانگین `rate×(1−loss)`)؛
  - کف = max(0.3×سقف اول، ۱ Mbit/s).
- داخل `govCapped`:
  - **تأیید**: هیچ episodeای به مدت ≥ ۲×فاصلهٔ قبلی رخ ندهد و ≥ ۵۰٪ تیک‌ها در سقف باشند ← `confirmed` و `holdPar`.
  - **episode در حالی که سقف محدودکننده است**:
    - اگر سقف به کف رسیده باشد: پس از ۲ episode، `lift`؛
    - وگرنه سقف به `×0.7` (هنوز در آزمون) یا `×0.8` (تأییدشده) پایین می‌آید، یا اگر کمتر است به `0.9×passedRate(15s)`.
  - **آزمون نشد**: دست‌کم ۳ episode زیر سقف، سقف ≤ ۰٫۴۵ × سقف اول، و فاصلهٔ میانگین ≤ ۱٫۴ × فاصلهٔ قبلی ← `lift`.
  - **بالا بردن**: هر ۴ ثانیه، اگر تمیز بوده و ≥۵۰٪ تیک‌ها در سقف، `×1.15`.
- `lift` ← `govNormal`. دورهٔ استراحت: `5min << min(lifts,4)` با سقف ۱ ساعت (`:486-491`).

### ۶-۳. حامل

| رخداد | واکنش | محل |
|---|---|---|
| ۱۵ ثانیه هیچ دریافتی | `ReadFrame` خطای `errDeadLink` می‌دهد و موتور دوباره dial می‌کند. | carrier.go:318-323 |
| خطای نوشتن روی سوکت | pacer `writeErr` را ذخیره می‌کند و **از حلقه خارج می‌شود**؛ ارسال‌های بعدی همان خطا را برمی‌گردانند. | pacer.go:421-428؛ carrier.go:271-273 |
| خطای خواندن در dial | `c.Close()` | dial.go:146-152، :166-172 |
| خطای گذرا در خواندن Listener | ادامه (به‌جز `net.ErrClosed`) | listen.go:98-123 |
| صف `rx` حامل در Listener پر است | datagram دور ریخته و `rxDropped` شمرده می‌شود. | carrier.go:398-405 |
| بازخورد کهنه یا جابه‌جا | دور ریخته می‌شود (`sendNanos ≤ last`، `dt<20ms`، کاهش `rxBytes`) | carrier.go:493؛ rate.go:509 |
| AEAD نامعتبر، replay یا قدیمی | بی‌صدا دور ریخته می‌شود. | carrier.go:456-459 |
| shard نامعتبر | `Invalid++` و دور ریختن | decoder.go:128-157 |
| گروه منقضی‌شده | `Lost += k − nDeliv` | decoder.go:281-290 |
| شکست تأیید | dial: `errConfirmFail`؛ سرور: `Close` | dial.go:247-248، :276-280 |
| دست‌دهی بی‌پاسخ | خطای «no handshake reply…» (کلید اشتباه هم به همین شکل بی‌صداست) | dial.go:220-222 |
| `auto`: کاوش بد | fallback به TCP (`noise`) | engine/carrier_udp.go:78-83 |

خطاهای ثابت: `errClosed` و `errDeadLink` و `errConfirmFail` (`carrier.go:61-63`)، `errShortDatagram` (`wire.go:21`)، و `errShort` و `errBadShard` و `errTooBig` (`fec/codec.go:20-24`).

---

## ۷. قالب قاب‌ها و پیام‌های پروتکل

1. **datagram داده** (`carrier.go:205-209`، `pacer.go:395-411`):
   - بی‌مهر: `[0x00][wireSeq:4][FEC hdr:9][len:2][seq:8][ct…]`
   - مهردار: `[0x02][wireSeq:4][sendStamp:4][FEC hdr:9][len:2][seq:8][ct…]`
   - `wireSeq` و `stamp` و سرآیند FEC **رمزنشده‌اند** (`REPORT.md:135-139`، مدل تهدید در `wire.go:99-116`).
2. **سرآیند FEC** (`fec/doc.go:22-29`، `codec.go:33-52`): `[group:4][index:1][k:1][r:1][shardSize:2]`. در shard داده k=r=size=0 است. پس `IsParity` یعنی `pkt[5]!=0`. اندازهٔ parity برابر بلندترین محتوای گروه است.
3. **datagram کنترل**: `[0x01][seq:8][ct…]`، بدون FEC و بدون pacer (`carrier.go:289-304`). برای `TypeFeedback`، `TypeAuth` و Ping/Pong/Close/PoolCtl/LinkStats.
4. **بازخورد** (`wire.go:31-56`)، ۴۱ بایت؛ همتای قدیمی ۳۶ بایت می‌فرستد:
   `sendNanos:8 | echoNanos:8 | echoDelayNanos:8 | rxDataBytes:8 | lossPPM:4 | owdTicks:4 | flags:1`
   - `fbOWD = 1`، `fbStamps = 2`.
   - گزارش ۳۶بایتی یعنی همتا مهر نمی‌فهمد. pacer تا وقتی `fbStamps` را نبیند بی‌مهر می‌فرستد (`wire_test.go`).
5. **کاوش** (`probe.go:23-31`): `"hs2P" | nonce:8 | seq:4 | sendNanos:8 | tag:16` با `tag = HMAC-BLAKE2s(shared, "hs2-udp-probe-v1"‖body)`. سرور همان بسته را عیناً برمی‌گرداند.
6. **دست‌دهی**: پیام ۱ و ۲ خام Noise IKpsk2، بدون tag. پیام ۱ با یک MAC کلیددار ۱۶بایتی شروع می‌شود (`core/handshake.go:186-211`).
7. **تأیید**: `TypeAuth` با تگ ۱۶بایتی (`auth.go:22-23`).
8. **padding (B6)**: درون AEAD؛ قاب‌های کوچک تا دستهٔ اندازه گرد می‌شوند و سقف آن innerMTU است (`carrier.go:774-779`). با `HS2_DG_PAD=0` خاموش می‌شود.

---

## ۸. متن دقیق لاگ‌های مهم

| متن | محل | معنی |
|---|---|---|
| `dg: policer suspected: %d loss episodes in %s, ~%s apart (%.0f%% loss on %d of %d carriers at once, no queue) — testing: whole pool capped at %.1f Mbit/s (%.0f%% of the %.1f Mbit/s that got through on average)` | udpcarrier/governor.go:418 | سقف آزمایشی روی کل استخر گذاشته شد. |
| `dg: policer confirmed: no loss episode for %s under the cap (they came every %s at the higher rate) — pool held at %.1f Mbit/s, parity sized for the path's own loss` | governor.go:428 | policer تأیید شد؛ parity به اتلاف تمیز محدود می‌شود. |
| `dg: policer: loss episode under the cap — lowered to %.1f Mbit/s` | governor.go:467 | سقف پایین آمد. |
| `dg: %s — the loss is not rate-dependent, so it is not a policer: cap lifted (detection rests %s)` با دلیل `loss episodes keep coming every ~%s under the cap (every ~%s before)` یا `loss episodes continue even at %.1f Mbit/s` | governor.go:492، :441، :451 | سقف برداشته شد و کشف برای مدتی متوقف است. |
| `dg: FEC maxed out on %d of %d carriers (mean parity %.0f%% of data) — the pool's loss estimate %.1f%% is past what it can repair (worst active carrier measured %.1f%%)` | engine/dgpool.go:1775 | دو نمونهٔ پشت سر هم با `FECAtCeiling`. |
| `dg: FEC no longer maxed out on any carrier (mean parity %.0f%% of data)` | engine/dgpool.go:1781 | پس از ۵ نمونه بدون سقف. |
| `transport: UDP selected (probe loss %.0f%%, rtt %s)` | engine/carrier_udp.go:74 | |
| `transport: UDP probe ok but dial failed (%v); falling back to TCP` | engine/carrier_udp.go:77 | |
| `transport: UDP unusable (reachable=%v loss %.0f%%); using TCP` | engine/carrier_udp.go:79 | |
| `transport: UDP probe error (%v); using TCP` | engine/carrier_udp.go:81 | |
| `udpcarrier: no handshake reply from %s (the path drops it, the other server is not running, or its shared_key differs)` | udpcarrier/dial.go:222 | خطای dial. |
| `udpcarrier: peer failed key confirmation` / `udpcarrier: link idle past deadline` / `udpcarrier: carrier closed` | carrier.go:61-63 | |
| `udpcarrier: invalid source IP %q` | srcaddr.go:19 | |
| `dial failed: %v (retry in %s)` | engine/engine.go:70 | موتور تک‌حاملی. |

---

## ۹. گزینه‌های پیکربندی و متغیرهای محیطی

**پیکربندی** (`/etc/hs2/config.json`، `cmd/hs2/main.go`):

- `carrier`: `udp` | `auto` | `dgtun` (`:405-424`).
- `mtu`: در این سه حالت پیش‌فرض ۱۲۸۰ است (`:367-369`، `:797-800`، `:831-834`).
- `bind_local_ip`: IP مبدأ برای dial، کاوش و fallback (`dial.go:67-73`، `carrier_udp.go:63`، `srcaddr.go`).
- `mode` و `reverse`: چه کسی dial کند (`main.go:430`، `:907`).
- `encap` و `proto` (فقط dgtun): کپسوله‌سازی (`main.go:853`، `dial.go:19-23`).
- `max_links` و envelope استخر در dgtun بیرون از این زیرسیستم است (autopilot؛ `main.go:854`).

**متغیرهای محیطی**:

| متغیر | اثر | محل |
|---|---|---|
| `HS2_FAIR_SHARE=0` | قواعد استخر و قاعدهٔ stage را خاموش می‌کند (رفتار قدیمی). | rate.go:309 |
| `HS2_DG_PAD=0` | padding دسته‌اندازه را خاموش می‌کند. | carrier.go:20 |
| `HS2_ICMP_CAMO=1` | لرزش بازخورد، آهنگ ۰٫۷ ثانیه در بی‌کاری و لرزش قطعی زمان probe. باید روی **هر دو سر** تنظیم شود. | carrier.go:35؛ rate.go:355-363 |
| `HS2_RAW_BATCH=0` | sendmmsg/recvmmsg را خاموش می‌کند (روی UDP هم). | batch_linux.go:19 |
| `HS2_RAW_TX=0` | ارسال دسته‌ای Listener را زیر قفل Go نگه می‌دارد. | batch_linux.go:26 |
| `HS2_FEC_SWEEP=1` | فقط آزمون: اجرای sweepها. | fec/sweep_test.go:91، :124 |
| (موتور) `HS2_DG_FQ=0`، `HS2_TUN_OFFLOAD=0`، `HS2_TUN_RCVBUF` | صف عادلانه، offload و بافر TUN در dgtun | engine/dgfq.go، main.go:840، engine/dgforward.go:75 |
| — | `SetHostSaturated` (از اندازه‌گیر CPU در status) روی اعتبار ۵۰ms اثر می‌گذارد. | cmd/hs2/status.go:330؛ pacer.go:126-130 |

---

## ۱۰. آزمون‌ها: چه چیزی تضمین می‌شود

**udpcarrier: کارکرد پایه و امنیت**
- `TestRoundTrip`، `TestManyFrames` (۵۰۰ قاب بدون اتلاف)، `TestDatagramPaddingTransparent` (همهٔ اندازه‌ها از ۰ تا ۱۲۰۰ دقیقاً برمی‌گردند)، `TestRoundTripListenAddrs` (v4، v6 و wildcard؛ دسته‌ای فقط IPv4).
- `TestMITMRejected`، `TestWrongKeyRejected`، `TestReplayRejected` (رله‌ای که همه را تکرار می‌کند: هیچ payloadی دو بار تحویل نمی‌شود)، `TestProbeReachable`، `TestProbeWrongKeyNoReply`، `TestEncapFramingKeyedBySecret`.
- `TestWildcardListenerRepliesFromTargetedIP` (pktinfo)، `TestDialFromRejectsInvalidSourceIP`.
- `TestCarrierOverhead` (بزرگ‌ترین datagram دقیقاً برابر `innerMTU+56` است، حتی با loss=0.4)، `TestInnerMTUFor`.
- `TestListenerStalledCarrierDoesNotBlockOthers`: حامل گیرکرده بقیه را قفل نمی‌کند.

**pacer**
- `TestPacerFastLane`: بستهٔ فوری حداکثر پس از یک datagram بیرون می‌رود.
- `TestPacerLaneCounters`.
- `TestPacerBatches`: ترتیب `wireSeq` درست است، دسته ≤۵ در ۱۶ Mbit/s، و سریع‌تر از نرخ نیست.
- `TestPacerDiag`.
- `TestPacerLateCreditOnlyWhenStageLimited`: بدون پرچم stage ≤۰٫۶ از اجازه، با پرچم ۰٫۷۵ تا ۱٫۱۵.
- `TestPacerLateCreditDataLane`، `TestPacerBurstCapHostSaturated`، `TestSetHostSaturated`، `TestPacerStampsOnlyForStampingPeer`.

**سیم و اتلاف**
- `TestFeedbackWireCompat` (۳۶ و ۴۱ بایت)، `TestStampArithmetic` (سرریز ۳۲بیتی).
- `TestWireLossInOrderIsZero`، `TestWireLossReorderIsNotLoss`، `TestWireLossReportsRealLoss` (±۱۰٪)، `TestWireLossDeepReorderIsBounded`، `TestWireLossPPMWindowed`.

**کنترل نرخ: شبیه‌ساز تک‌حامل** (`rate_sim_test.go`؛ زمان شبیه‌سازی‌شده؛ این شبیه‌ساز برآوردگر اتلاف قدیمی high-water را دارد)
- `TestRateSimMatrix`: ۲ تا ۵۰۰ Mbit/s، RTT از ۲۰ تا ۳۰۰ms، حالت‌های تمیز، iid ۵٪ و انفجاری ۲۶٪. بهره‌وری ≥۹۵٪ یا ≥۹۰٪، p95 صف ≤ ۳۰، ۴۵ یا ۱۵۰ms، و بافر کم‌عمق ۱۵ms با میانگین ≤۱۳ms.
- `TestRateSimCapacityChange`، `TestRateSimAppLimited` (نرخ نهایی ≤۱۵ Mbit/s وقتی برنامه ۵ Mbit/s می‌دهد)، `TestRateSimSlowPathNoRunaway` (پشتیبان `startCap`)، `TestRateSimRTTFallback`، `TestRateSimAppLimitedStartThenBurst`، `TestRateSimCrossTraffic`، `TestRateSimIdleThenBurstRampsFast` (≥۵۰٪ در ۲٫۵ ثانیهٔ اول پس از ۳۰ ثانیه بی‌کاری).

**کنترل نرخ: شبیه‌ساز استخر** (`rate_pool_sim_test.go`)
- `TestPoolSimLateCarriersShare` (Jain ≥۰٫۹، بهره‌وری ≥۸۵٪، میانگین صف ≤۲۵ms، بدون drop)، `TestPoolSimLightCarrierKeepsItsShare`، `TestRateSimOfferAboveCapacityLeavesStartup`، `TestPoolSimSlowSeparatePath`، `TestPoolSimLightCarrierStaysInStartup`، `TestPoolSimSlowPathBigShare`، `TestPoolSimLightCarrierLaterRamp`، `TestPoolSimConvergence`، `TestRateSimBottleneckAppearsAfterFastPeriod`، `TestNextProbeMonotonic`، `TestPoolSimLightCarrierOwnPathLeavesStartup`، `TestPoolSimLightCarrierDeepOwnBufferLeavesStartup`، `TestPoolSimLightCarrierUnderCrossQueue`، `TestRateControlLightAgainstShare`، `TestPoolSimEightCarriers`، `TestRateControlRTTSamples`، `TestRateControlDeepQueueNeedsShortfall`.

**stage و CPU** (`stage_test.go`، `rate_cpu_pool_test.go`)
- `TestRateControlStageLimitedLeavesStartup` (≤۳۰ گزارش، اجازه ≤۲٫۲۵ برابر)، `TestRateControlStageCapAndRegrow`، `TestRateControlStageIgnoredWhenLimitedOrCapped`، `TestRateControlStageFromPacerWait`، `TestRateControlStageNotInBaseProbe`، `TestRateControlStageNotRefreshedByProbes`، `TestRateControlStageFlagExpiresWithoutFeedback`.
- `TestPoolSimCPUBoundSender`، `TestPoolSimCPUBoundStalePeak`.

**camo**
- `TestCamoProbeOffsetDeterministicBounded`، `TestCamoProbeSynchronisedButNotFixedGrid`، `TestCamoJitterBounds`.

**Governor**
- `TestGovernorCapsAPolicer` (policer ۶۰ و پیشنهاد ۱۵۰ ← ۴۰ تا ۶۴ Mbit/s، تأییدشده)، `TestGovernorIgnoresRandomLoss`، `TestGovernorIgnoresSteadyRandomLossOnAllCarriers`، `TestGovernorIgnoresLossWhenNotPushing`، `TestGovernorIgnoresCongestionLoss`، `TestGovernorLiftsCapWhenLossIsNotRateDependent` (≤۴۰ ثانیه، استراحت رو به رشد، parity هرگز نگه داشته نمی‌شود)، `TestGovernorBucketHoldsThePoolToItsCap` (۹ تا ۱۱٫۵ برای سقف ۱۰)، `TestGovernorBusyQueueAndShareHold`.

**آزمایشگاه درون‌فرایندی** (`lab_test.go`؛ در حالت `-short` رد می‌شوند)
- `TestLabBursty26`: اتلاف سیم ≥۱۲٪، باقیمانده ≤ ۴۰٪ اتلاف سیم و ≤۱۴٪، goodput ≥۶٫۵ Mbit/s روی ۲۰ Mbit/s، jitter ≤۳۲۰ms.
- `TestLabAdaptiveStep`: r/k ≥ ۰٫۳ پس از جهش.
- `TestLabOverheadLowVsHigh`.

**fec**
- `TestNoLossDeliversEverythingOnce` (سربار کف ۵ تا ۲۰٪)، `TestRecoversAtPathLoss` (iid ۲۶ و ۳۱٪ ← باقیمانده ≤۲٪)، `TestInterleavingBeatsBursts`، `TestNetemCorrelatedLoss`، `TestFlushBoundsWait`، `TestAdapterTracksSteps`، `TestAdaptiveOverheadFollowsLoss`، `TestMalformedDoesNotPanic`، `TestSmallGroupsAndOrder`، `TestEncoderReadersNotBlockedByEmit`، `FuzzDecode`، و `TestSweep` و `TestSweepAdapter` (ابزار تنظیم؛ قبول یا رد ندارند).

**موتور** (`engine/carrier_udp_test.go`)
- `TestAutoSelectsUDPWhenGood`، `TestProbeSeesBlockedUDP`، `TestAutoFallsBackToTCP`.
- **آزمونی برای مسیر «قابل دسترس ولی پراتلاف» در `auto` (اتلاف بین ۰ و ۴۵٪) وجود ندارد.**

---

## ۱۱. «از قبل وجود دارد»

برای جلوگیری از دوباره‌کاری، این‌ها همین حالا پیاده شده‌اند:

1. Noise IKpsk2 روی datagram، با کلیدهای pinشده از رمز مشترک، MAC ارزان پیام ۱، cache پیام ۲، و تأیید کلید به سبک exporter/binding.
2. AEAD برای هر datagram، پنجرهٔ replay ۲۰۴۸تایی (`core/replay.go:20`)، و حذف تکراری در FEC.
3. Reed-Solomon درهم‌چیده که عمقش از روی نرخ تنظیم می‌شود (پنجرهٔ زمانی ثابت)، با parity تطبیقی بر اساس مدل دوجمله‌ای و هدف باقیماندهٔ ۱٪.
4. تحویل فوری shard داده؛ FEC فقط به بستهٔ گم‌شده تأخیر می‌دهد.
5. Adapter نامتقارن با حفظ اوج، کف و سقف.
6. برآوردگر اتلاف سیم که جابه‌جایی ترتیب را اتلاف حساب نمی‌کند.
7. OWD یک‌طرفه با مهر ۱۲۵µs و پشتیبان RTT برای همتای قدیمی. بازخورد خام و بدون pacing.
8. کنترل نرخ تأخیرمحور (هدف صف ۱۰ms)، با startup به سبک BBR، probe ظرفیت ۴٪، base probe هم‌زمان در کل استخر، جبران اتلاف تصادفی، کشف policer تک‌حاملی و سقف تحویل.
9. قواعد استخر: سهم عادلانه، حامل سبک، صف حامل‌های پرکار، حذف اوج کهنه.
10. قاعدهٔ stage برای CPU یا سوکت، اعتبار دیرکرد ۱۰ms و اعتبار اشباع ۵۰ms.
11. Governor برای کل استخر: کشف policer از روی episodeهای همزمان، سطل توکن مشترک، آزمون، تأیید، برداشتن سقف با استراحت نمایی، و محدود کردن parity زیر policer تأییدشده.
12. pacer سه‌صفه (اولویت parity، صف فوری با ضامن ترتیب)، صف با بودجهٔ زمانی، و فشار برگشتی به‌جای drop.
13. sendmmsg/recvmmsg روی UDP و سوکت‌های خام، و ارسال بدون قفل Go روی Listener.
14. pktinfo برای پاسخ از همان IPی که هدف بوده، و IP مبدأ قابل تنظیم.
15. `tryFeed` غیرمسدودکننده در Listener.
16. محاسبهٔ MTU برای هر کپسوله‌سازی، با آزمون دقیق سربار.
17. padding دسته‌اندازه، camo زمانی icmp (لرزش بازخورد و probe).
18. کاوش احرازشده روی همان پورت (بازتاب‌دهندهٔ باز نیست) و انتخاب خودکار UDP یا TCP هنگام dial.
19. (بیرون از این بسته، در استخر) صف عادلانه برای هر حامل، کشف حامل «mute» پس از ۱ ثانیه، بافر ترتیب TCP پیش از TUN، و autopilot تعداد حامل‌ها (`engine/dgfq.go`، `engine/dgpool.go:65-74`، `engine/linkmanager.go:2280-2281`).

---

## ۱۲. ایده‌هایی که امتحان و رد شده‌اند

| ایده | نتیجه یا دلیل رد | منبع |
|---|---|---|
| کنترل ازدحام اتلاف‌محور (مثل TCP) | با ۲۶٪ اتلاف هر انفجار را ازدحام می‌خواند و فرو می‌ریزد. | udpcarrier/doc.go:9-16؛ rate.go:11-18 |
| بازارسال | دست‌کم یک RTT برای هر اتلاف؛ در این نرخ اتلاف، کنترل‌گر را فرو می‌ریزد. | fec/doc.go:5-12 |
| عبور بازخورد از FEC و pacer | تأخیر و جابه‌جایی ناشی از بازسازی، برآورد نرخ و RTT را بی‌فایده کرد (باگ واقعی). بازخورد اکنون خام است. | REPORT.md:46-48؛ carrier.go:200-204 |
| تنظیم نرخ بر اساس goodput پس از FEC | یک دوره اتلاف جبران‌نشده نرخ را مارپیچ‌وار پایین می‌برد و درهم‌چینی را گرسنه می‌کرد؛ اکنون نرخ تحویل سیم ملاک است. | carrier.go:598-600 |
| برآوردگر high-water برای اتلاف سیم | جابه‌جایی ترتیب را تا ۱۰۰٪ «اتلاف شبح» نشان می‌داد، parity را به سقف می‌چسباند و Governor را فعال می‌کرد. | carrier.go:146-148؛ wireloss_test.go:5-11 |
| صف pacer با تعداد بستهٔ ثابت (۶۴) | فقط در یک نرخ کران زمانی است (۳۰۰ms در ۲ Mbit/s)؛ با کران ۲۰ms جایگزین شد. کف ۸ datagram در نرخ کف ۳۷۵ms بود. | pacer.go:70-75، :83 |
| سقف دو datagramی برای سطل توکن | هر حامل را به حدود ۲۰ Mbit/s محدود می‌کرد. | pacer.go:302-306 |
| K بزرگ (۳۲، پیش‌فرض fec) برای حامل | برای درهم‌چینی، pps بسیار بالاتری لازم دارد؛ K=8 انتخاب شد. | carrier.go:163-166 |
| پنجرهٔ FEC برابر ۱۰ms یا ۸۰ms | ۱۰ms باقیماندهٔ ۵ تا ۷٪ داشت؛ ۸۰ms کمی بهتر بود ولی بستهٔ بازسازی‌شده را تا ۸۰ms دیر می‌رساند. | fec/encoder.go:43-46؛ fec/README.md:36 |
| حفظ اوج با ۰٫۷۵ × اوج | حدود ۳۰٪ سربار بیشتر برای حدود ۱٪ باقیماندهٔ کمتر. | fec/adapt.go:51-55؛ README.md:40 |
| شمردن گزارش‌های بی‌کار در پشتیبان startup | پس از حدود ۱۰ ثانیه بار سبک startup تمام می‌شد و جریان بعدی از کف بالا می‌رفت. | rate.go:632-638؛ rate_sim_test.go:556-564 |
| reset کمینه به مقدار فعلی پس از انقضا | دام «تازه شدن با یک نمونهٔ صف‌دار»؛ minFilter دوسطلی جایش را گرفت. | rate.go:398-401 |
| رد همهٔ نمونه‌های RTT بالای ۸×srtt | مسیر بالای ۴۰۰ms هرگز اندازه‌گیری نمی‌شد. | ../CHANGELOG.md:1331-1338؛ rate.go:465-467 |
| کف ۰٫۴× اوج حتی وقتی حامل از اجازه استفاده نمی‌کند | پس از سرعت ۹۰۰ Mbit/s در گلوگاه ۳۰ عملاً pacing نداشت (ping 122/475ms). | rate.go:82-85، :546-550؛ CHANGELOG:1298-1309 |
| رشد سهم عادلانه وقتی صف نیست | حامل روی مسیر جدای کندتر را به بافرش هل می‌داد (۱۰۰ تا ۲۰۰ms). | rate.go:97-100؛ CHANGELOG:1316-1323 |
| گام سهم عادلانه کوچک‌شونده با RTT | همگرایی در مسیر بلند بد بود (۰٫۶۳)؛ اکنون ۳٪ در هر گزارش است. | CHANGELOG:1386-1389 |
| انتظار صف عمیق بر اساس srtt | srtt خودش صف را دارد؛ صف به base تبدیل می‌شد و حامل برای همیشه در startup می‌ماند. | rate.go:662-664؛ CHANGELOG:1393-1400 |
| probe پایه برای هر حامل جداگانه | بقیه صف را پر نگه می‌داشتند و base با صف اندازه‌گیری می‌شد. | rate.go:101-105 |
| ضامن ترتیب ثابت ۱۰۰ms برای صف فوری | در نرخ کف یا زیر سقف policer، بسته‌ها از هم جلو می‌زدند؛ اکنون شمارندهٔ صف است. | CHANGELOG:1201-1203 |
| تشخیص policer در هر حامل به‌تنهایی | بقیهٔ حامل‌ها جمع را بالای حد نگه می‌دارند؛ Governor استخری ساخته شد. | governor.go:13-20 |
| سقف Governor روی حامل‌های غیر pushing | سقف روی ترافیک اتصال مجدد (۲۷ Mbit/s) گذاشته می‌شد. | governor.go:319-323؛ governor_test.go:131-134 |
| episode = اتلاف همزمان بدون مقایسه با میانه | اتلاف ثابت ۱۰٪ سقف می‌گذاشت (goodput از ۲۹ به ۲۳). | governor_test.go:113-116 |
| parity بیشتر زیر policer | فقط بایت بیشتری به policer می‌داد؛ اکنون به اتلاف تمیز محدود است. | carrier.go:517-523 |
| `feed` مسدودکننده در Listener | حدود ۷۰ ثانیه خاموشی کامل یک استخر icmp. | carrier.go:392-397؛ hol_test.go:12-17 |
| `LastRx` بر اساس unix time | گام NTP همهٔ حامل‌ها را ساکت نشان می‌داد؛ اکنون یکنوا (monotonic) است. | carrier.go:345-348 |
| قفل Encoder برای `SetLoss` و آمار | بازخورد و وضعیت پشت pacer منتظر می‌ماندند. | fec/encoder.go:85-89؛ CHANGELOG:1646-1651 |
| کاملاً ساکت شدن حامل بی‌کار در camo | آشکارساز mute (۱ ثانیه) را به نوسان می‌انداخت؛ آهنگ ۰٫۷ ثانیه جایگزین شد. | carrier.go:26-31، :567-573 |
| CA3: نگه داشتن بسته‌ها زیر آستانهٔ اندازه | توان را ۶ تا ۷ برابر کم می‌کند و خودش نشانه می‌شود؛ ساخته نشد. | CHANGELOG:1500-1504 |
| CA4: شمارندهٔ echo seq یا جفت کردن دو جهت | هر گونه‌اش نشانهٔ بدتری می‌ساخت؛ ساخته نشد. | CHANGELOG:1504-1512 |
| صف ارسال عمیق‌تر هنگام اشباع | هیچ تغییری ایجاد نکرد. | CHANGELOG:1552-1553 |
| بستن کامل شکاف حدود ۱۴٪ با رها کردن clamp نرخ | پس از آزاد شدن CPU، بافر مسیر مشترک را پر می‌کند (صدها drop). | CHANGELOG:1553-1557 |
| ارسال دسته‌ای روی Listener IPv6 | همهٔ datagramها حذف می‌شدند؛ اکنون فقط IPv4. | CHANGELOG:1459-1461؛ batch_linux.go:105-107 |
| گزینه‌های `pt`، `wb` و `pb` در شبیه‌ساز CPU (کران sojourn برای صف داده، صبر نویسنده پیش از pop، بودجه بر اساس نرخ ارسال اخیر) | در شبیه‌ساز تعریف شده‌اند ولی هیچ آزمونی روشنشان نمی‌کند و در pacer پیاده نشده‌اند. **دلیل رد مستند نیست (نامطمئن).** | rate_cpu_sim_test.go:41-48، :503-551 |

---

## ۱۳. محدودیت‌های شناخته‌شده و مشاهده‌ها

### ۱۳-۱. محدودیت‌های اعلام‌شده در خود مخزن

- fallback فوری نیست: تشخیص بعد از ۱۵ ثانیه، بعد dial مجدد، کاوش و TCP. کاوش پیوسته «کار آینده» است (`REPORT.md:126-130`).
- `wireSeq`، مهر و سرآیند FEC در متن آشکارند؛ برای مقاومت در برابر DPI در حد reality/TLS نیست (`REPORT.md:135-139`).
- کنترل نرخ «BBR-lite» است و در برابر جریان‌های رقیب در گلوگاه مشترک سخت‌گیرانه آزموده نشده (`REPORT.md:140-142`).
- باقیماندهٔ اتلاف در ۲۶٪ حدود ۳ تا ۷٪ است و گران است (`REPORT.md:143-145`). در `TestLabBursty26` باقیمانده حدود ۶ تا ۹٪ است (`lab_test.go:223-226`).
- فهرست «Pre-existing, unchanged» (`../CHANGELOG.md:1424-1457`):
  - زیر policer ثابت بدون episode، استخر حدود ۲٫۵ برابر آنچه عبور می‌کند می‌فرستد و parity به سقف می‌رسد؛
  - ۱۶ حامل روی ۸ Mbit/s به کف فرو می‌ریزند؛
  - پس از startup، بالا رفتن دوباره فقط ۴٪ در هر گزارش است؛
  - RTT ۳۰۰ms با ۲۶٪ انفجاری گاهی فرو می‌ریزد (۴ از ۳۰ seed)؛
  - حامل سریع، پیدا شدن گلوگاه را ۱ تا ۲ ثانیه بدون pacing می‌گذراند؛
  - ۸ حامل روی ۱۶ تا ۲۴ Mbit/s در RTT ۸۰ms: جبران اتلاف، drop بافر پر را «اتلاف تصادفی» می‌خواند و استخر روی بافر پر می‌ماند. «اصلاحش جایش در جبران اتلاف است و به تغییر جداگانه واگذار شد.»
- کنترل‌گر تأخیرمحور کنار جریان اتلاف‌محوری که صف را بالای حدود ۱۰۰ms نگه دارد له می‌شود (`CHANGELOG:1446-1456`).
- شکاف حدود ۱۴٪ عمداً در CPU اشباع باقی مانده است (`CHANGELOG:1550-1557`).

### ۱۳-۲. مشاهده‌ها (از خواندن کد؛ بدون پیشنهاد تغییر)

1. **مشاهده: آستانهٔ `auto` روی اتلاف رفت‌وبرگشت اعمال می‌شود.** کاوش فقط وقتی «دریافت‌شده» است که درخواست و پژواک هر دو برسند (`probe.go:150-156`، `:179-180`). با اتلاف یک‌طرفهٔ متقارن ۲۶٪، اتلاف رفت‌وبرگشت حدود ۱−۰٫۷۴² ≈ ۴۵٪ است (محاسبه‌شده)، یعنی درست روی آستانهٔ `0.45` (`carrier_udp.go:24`). مستندات این آستانه را با اتلاف یک‌طرفهٔ سیم مقایسه کرده‌اند (`REPORT.md:90-92`). همچنین:
   - ۱۶ کاوش یعنی دانه‌بندی ۶٫۲۵٪؛
   - کاوش‌ها در حدود ۱۲۸ms پخش‌اند و یک انفجار طولانی می‌تواند بیشترشان را ببرد.
2. **مشاهده:** وقتی `auto` روی TCP افتاد، تا وقتی آن حامل زنده است دوباره کاوش نمی‌کند (`carrier_udp.go:14-19`). حتی اگر UDP بعداً خوب شود، روی `noise` می‌ماند.
3. **مشاهده: توضیح‌های کهنه یا ناهمخوان با کد.**
   - `carrier.go:163` می‌گوید «40 ms window» ولی `Window` برابر ۶۰ms است (`:167`).
   - `carrier.go:220-221` می‌گوید «48» ولی `wireConfirmWin=64` (`:224`).
   - `carrier.go:615` از `wireReorderWin` می‌گوید در حالی که تأیید با `wireConfirmWin` است.
   - `dial.go:38` و `cmd/hs2/main.go:366` می‌گویند `CarrierOverhead` ۵۲ است، ولی ۵۶ است (`mtu.go:11`).
   - `fec/README.md:35-41` و `fec/doc.go` پیش‌فرض‌ها (K=32، 30ms) را توضیح می‌دهند، در حالی که حامل K=8 و 60ms را می‌گذارد.
   - `REPORT.md:27` کنترل نرخ را «startup نمایی و سپس چرخهٔ gain برای probe/drain» توصیف می‌کند که با کد فعلی نمی‌خواند.
   - توضیح `governor.go:486` («80 min») با سقف `govRestMax=1h` نمی‌خواند.
   - README (`../README.md:584-590`) و CHANGELOG (`:1481-1483`) می‌گویند حامل بی‌کار در camo «ساکت می‌شود»، ولی کد عمداً هر حدود ۰٫۷ ثانیه می‌فرستد (`carrier.go:567-579`).
   - README (`../README.md:757-759`) می‌گوید fallback در `auto` به «tcp/TLS» است، ولی کد به `noise` (TCP) برمی‌گردد (`carrier_udp.go:86-90`؛ نصب‌کننده هم می‌گوید «tcp fallback»: `../install.sh:2659`).
4. **مشاهده: خطای نرم ارسال روی UDP کل pacer را از کار می‌اندازد.**
   - `udpConnWriteBatch` و `udpWriteBatchTo` اولین خطا را برمی‌گردانند (`batch_linux.go:44-47`، `:220-222`)؛ pacer با هر خطای نوشتن `writeErr` را ثبت می‌کند و **از حلقه خارج می‌شود** (`pacer.go:421-428`).
   - در مقابل، کپسوله‌سازی خام فقط datagram ردشده را دور می‌اندازد (`encap/raw_linux.go:620-634`؛ CHANGELOG:1242-1247 همین را برای «udp sockets» هم ادعا می‌کند).
   - پس از خروج pacer، نویسنده‌ای که داخل `enqueueLane` منتظر `room` یا کانال پر است تا `Close` بیدار نمی‌شود (`pacer.go:193-226`). بازخوردِ همین سمت همچنان مستقیم با `c.write` می‌رود (`carrier.go:303`).
   - **نامطمئن:** اینکه این وضع در عمل چقدر پیش می‌آید و آیا آشکارساز mute استخر یا سازوکار دیگری آن را می‌گیرد، بررسی اجرایی نشد.
5. **مشاهده: وابستگی به ساعت دیواری.**
   - ترتیب بازخورد با `sendNanos` ساعت دیواری همتا سنجیده می‌شود (`carrier.go:493-496`). یک گام رو به عقب در ساعت همتا، گزارش‌ها را به اندازهٔ آن گام «کهنه» می‌کند و نرخ و FEC منجمد می‌مانند.
   - مهر OWD هم بر اساس `UnixNano` است (`wire.go:119`؛ `pacer.go:403`؛ `carrier.go:434`) و برخلاف مسیر RTT (`rate.go:461-480`) هیچ نگهبانی ندارد (`rate.go:485-491`). گام رو به جلو در ساعت گیرنده (یا رو به عقب در ساعت فرستنده) تا حدود ۵ تا ۱۰ ثانیه مثل صف ایستاده خوانده می‌شود.
   - **نامطمئن:** وقوع در عمل (NTP معمولاً slew می‌کند، نه step).
6. **مشاهده: تأخیر برآوردگر اتلاف.** اتلاف فقط پس از رسیدن ۶۴ datagram بعدی تأیید می‌شود (`carrier.go:652-658`). در ۱۰۰ pps یعنی حدود ۶۴۰ms. در انتهای یک انتقال، seqهای حل‌نشده تا ترافیک بعدی معلق می‌مانند و بعد در گزارشی دیگر ثبت می‌شوند.
7. **مشاهده:** شبیه‌سازهای نرخ (`rate_sim_test.go:243-251`) هنوز اتلاف را با روش high-water قدیمی گزارش می‌کنند، نه با `trackWire`.
8. **مشاهده: سربار FEC برای ترافیک کم.** گروهی که با پنجرهٔ ۶۰ms بسته می‌شود دست‌کم یک parity دارد. با k=1 سربار ۱۰۰٪ و با k=2 سربار ۵۰٪ است (محاسبه‌شده). ACK و ترافیک تعاملی کم‌حجم عملاً دو برابر بسته می‌سازند. این طراحی عمدی است (`fec/doc.go:51-55`).
9. **مشاهده:** `FECAtCeiling` در `est ≥ 0.5` روشن می‌شود (`carrier.go:806`)، ولی r=12 (بیشینه برای K=8) از `est ≥ 0.365` به بعد برقرار است (محاسبه‌شده). در بازهٔ ۰٫۳۶۵ تا ۰٫۵، parity به سقف رسیده ولی وضعیت «maxed out» گزارش نمی‌شود.
10. **مشاهده:** `maxQueueGain=1.1` (`rate.go:246`) دست‌نیافتنی است، چون بیشینهٔ ضریب `1 + targetQueue/τ ≤ 1.04` است (`rate.go:780-782`).
11. **مشاهده (محاسبه‌شده، نامطمئن در اثر عملی):** پنجرهٔ replay ۲۰۴۸ seq است (`core/replay.go:20`). بسته‌ای که FEC آن را با تأخیر بیش از ۲۰۴۸ قاب مهروموم‌شده بازسازی کند، «قدیمی» رد می‌شود. با TTL برابر ۲۲۰ms این فقط بالای حدود ۹ هزار قاب در ثانیه برای هر حامل معنا دارد (حدود ۱۰۰ Mbit/s با ۱٫۳ KB).
12. **مشاهده:** شمارندهٔ `lifts` در Governor هرگز صفر نمی‌شود (`governor.go:486-491`). پس از ۴ بار برداشتن سقف، هر استراحت تا پایان عمر استخر ۱ ساعت است. سقف «تأییدشده» فقط از مسیر کف برداشته می‌شود (`:447-454`) و در غیر این صورت فقط بالا می‌رود (`:470-472`). عمدی بودنش **نامطمئن** است.
13. **مشاهده:** demux در Listener بر اساس `addr:port` مبدأ است (`listen.go:134-147`). هیچ مهاجرت اتصالی برای NAT rebinding وجود ندارد؛ datagramهای نشانی جدید به‌عنوان دست‌دهی امتحان و بی‌صدا رد می‌شوند. اگر یک dial دوباره از همان `addr:port` بیاید، پیام ۱ جدیدش (که با `m1` cacheشده فرق دارد) به حامل قدیمی داده می‌شود تا آن حامل بسته شود. در کپسوله‌سازی‌های خام اینکه «نشانی» دقیقاً چیست را بررسی نکردم (**نامطمئن**).
14. **مشاهده:** کاوش، پژواک تکراری را دو بار می‌شمارد، چون `sentAt[seq]` پاک نمی‌شود (`probe.go:150-155`). اتلاف کمتر از واقع برآورد می‌شود.
15. **مشاهده:** نرخ pacer، parity را هم شامل می‌شود و کنترل نرخ از FEC بی‌خبر است. goodput تقریباً برابر rate × k/(k+r) است؛ در ۲۶٪ اتلاف بیش از نیمی از پهنای باند pace‌شده parity است (REPORT: سربار ۱۲۴٪).
16. **مشاهده:** `dec.Expire` فقط وقتی datagram داده برسد اجرا می‌شود (`carrier.go:437-440`)؛ در بی‌کاری، گروه‌ها و آمار `Lost` معوق می‌مانند.
17. **مشاهده:** قاب‌های کنترلی (بازخورد، keepalive، تأیید) FEC ندارند. آشکارساز mute در استخر، ۱ ثانیه سکوت (یعنی ۱۰ گزارش پیاپی گم‌شده) را «قطع» می‌داند (`engine/dgpool.go:65-74`). روی مسیری با انفجارهای چندثانیه‌ای ۷۰٪ (که `fec/adapt.go:53-54` از آن نام می‌برد)، احتمال mute کاذب صفر نیست. **نامطمئن**، چون این آشکارساز فقط وقتی حکم می‌کند که حامل دیگری هنوز بشنود.
18. **مشاهده:** ارسال و دریافت دسته‌ای Listener فقط روی IPv4 است و IPv6 تکی کار می‌کند (`batch_linux.go:105-107`). کاوش همیشه UDP ساده است (`probe.go:99-107`).

---

## ۱۴. ارجاع به زیرسیستم‌های دیگر

**این زیرسیستم صدا می‌زند:**

- `core`: `NewInitiator`/`NewResponder`/`NewSession`، `StaticFromSeed`، `AppendDatagram`/`OpenDatagram`/`DatagramLen`/`PadDatagramTarget`، `Exporter`، `HandshakeBinding`/`PeerStatic`، و پنجرهٔ replay (`core/session.go:141-182`، `core/datagram.go:75`، `core/handshake.go:186`).
- `encap`: `encap.Dial`/`encap.Listen`/`encap.Overhead`/`Options` (`dial.go:88`، `listen.go:69`، `mtu.go:31`).
- `fec` از درون `carrier.go` و `pacer.go`.
- `mmsg` از درون `batch_linux.go`.
- `klauspost/reedsolomon` (`fec/codec.go:60-81`)، `golang.org/x/crypto/blake2s` و `golang.org/x/sys/unix`.

**صدا زده می‌شود از:**

- `engine/carrier_udp.go`: `udpDialer`، `udpListener`، `autoDialer`، `autoListener`؛ `Probe`/`ProbeFrom`/`DialFrom`/`Listen`.
- `engine/dgcarrier.go`: `DialCfg`/`ListenCfg` برای هر حامل استخر dgtun.
- `engine/dgpool.go`:
  - `NewGovernor` (`:600`) و `Run` (`:2216`، `:2253`)؛
  - `AttachGovernor` (`:664`)؛
  - `SendUrgent`، `LaneMark` و `LaneDrained` (`:236-239`، `:419-449`؛ صف عادلانه در `engine/dgfq.go`)؛
  - `NoteQueueDrop` (`:381-384`)؛
  - `LastRx` (`:265`) برای آشکارساز mute؛
  - `Warm` (`:109`)، `TryReadFrame` (`:733-754`)، `Encap` (`:888-893`)؛
  - `Stats` (`:1699-1781`، `:1851-1896`) برای وضعیت (`P`/`S`/`C`، FEC، policer، share).
- `cmd/hs2/main.go:405-424`، `:795-904` (انتخاب حامل و MTU)، و `cmd/hs2/status.go:330` (`SetHostSaturated`).
- `encap/raw_linux.go` و `encap/rawtx_linux.go` از `mmsg` (`NewBatch`/`Send`/`SendNoLock`/`Recv`/`Inet4`).
- `lab/dglab/main.go` و `lab/netsim` (آزمون‌ها).

**ارتباط با `l3mtcp`:** مستقیم هیچ. `l3mtcp` از `engine/stream*.go` و `tlscarrier` روی TCP یا TLS استفاده می‌کند. نه FEC دارد، نه این pacer، نه این کنترل نرخ. تنها اشتراک، ساختار گزارشی `PoolStats` است (`engine/linkmanager.go:2256-2291`) که فیلدهای FEC و policer آن فقط در dgtun پر می‌شوند (`engine/dgpool.go:1715-1757`).
