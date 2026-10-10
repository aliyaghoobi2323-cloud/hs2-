# حامل TLS، احراز، استتار و هستهٔ رمزنگاری

> دامنه: `tlscarrier/*`، `obfs/*`، `engine/shape.go`، `core/*`، `reality/*`، `engine/carrier_reality.go`، `engine/carrier_noise.go` (و `engine/carrier_tcp.go` که دست‌دهی Noise را روی TCP انجام می‌دهد)، به‌اضافهٔ تکه‌هایی از `cmd/hs2` که گواهی، صفحهٔ پوششی و تنظیم سوکت را به این لایه وصل می‌کنند.
> مبنا: شاخهٔ فعلی برابر `main` (ثبت `0812bc9`). همهٔ path:lineها نسبت به `/home/user/hs2-/hs2-src/` هستند، مگر خلافش گفته شود (`CHANGELOG.md` و `README.md` در ریشهٔ مخزن هستند).
> آزمون‌های `tlscarrier`، `core`، `reality` و آزمون‌های شکل‌دهی `engine` (`TestShaped*`، `TestNewSessionShapes*`) و `TestCert*`/`TestCover*` در `cmd/hs2` روی این نسخه اجرا شدند و همه سبز بودند (Go 1.27.0).

---

## ۱. نقش و جایگاه در کل سیستم

- **تونل اصلی کاربر (`l3mtcp`) و `mtcp`/`tls` هیچ‌کدام از رمزنگاری `core` استفاده نمی‌کنند.** امنیتِ این سه حامل فقط از خودِ TLS 1.3 می‌آید، به‌اضافهٔ یک احراز دوطرفه با کلید مشترک که به همان نشست TLS گره خورده است (`tlscarrier/auth.go`). یک لایهٔ AEAD دوم عمداً اضافه نشده (`BUILD.md:179-180`؛ `reality/carrier.go:10-15`؛ `engine/carrier_reality.go:15-20`).
- چیدمان لایه‌ها برای هر لینک mtcp (و l3mtcp)، از پایین به بالا:
  1. TCP: سوکت تنظیم‌شده با `tuneTCP` (BBR، `TCP_NOTSENT_LOWAT`=32KiB، `TCP_USER_TIMEOUT`=20s، `TCP_NODELAY`). سمت شماره‌گیر keepalive هسته هر 3 ثانیه است (`tlscarrier/tune_linux.go:32-51`، `tlscarrier/carrier.go:186-190`).
  2. TLS 1.3: سمت شماره‌گیر با utls و اثرانگشت `HelloChrome_133` (`tlscarrier/carrier.go:192`)، و سمت شنونده با `crypto/tls` استاندارد Go و گواهی واقعی Let's Encrypt (`tlscarrier/server.go:72-88`).
  3. یک رفت‌وبرگشت احراز داخل TLS: رکورد احراز کلاینت و سپس اثبات سرور، هر دو با پرکنندهٔ تصادفی به اندازهٔ HTTP (`tlscarrier/auth.go:18-51`).
  4. `shapedConn`: هر نوشتن را به رکوردهایی با طول نمونه‌برداری‌شده از یک توزیع ثابت «شبیه HTTPS» می‌شکند (`engine/shape.go:11-32`، `obfs/shaper.go:35-56`).
  5. `meteredConn` (برای سنجش سلامت؛ در لبه و در خروجیِ مستقیم هر دو فعال است) و بعد `watchConn` (`engine/stream.go:172-203`).
  6. smux نسخهٔ 2 با keepalive تصادفیِ 4 تا 8 ثانیه برای هر نشست (`engine/mtcp_link.go:267-284`).
  7. جریان‌های smux: بایت اولِ هر جریان «نوع» آن را می‌گوید (`engine/stream.go:33-48`). در l3mtcp یک جریان `kindL3` بسته‌های IP دستگاه hs0 را در قالب قاب `tlscarrier` حمل می‌کند (`[ftype:1][len:3][pad:3]`، `engine/l3_link.go:210,235`؛ `engine/stream.go:205-241`).
- سرور TLS در حالت مستقیم «خارج» است و در حالت معکوس (`reverse`) «ایران» (`cmd/hs2/main.go:489-501, 520-547`). نقش TLS و نقش smux از هم جدا هستند: لبه همیشه کلاینت smux است، چه TLS را خودش شماره‌گیری کرده باشد چه پذیرفته باشد (`engine/mtcp_link.go:294-307`).
- `core` (Noise IKpsk2، قاب AEAD با طولِ ماسک‌شده، پنجرهٔ ضد replay) هستهٔ رمزنگاریِ **حامل‌های datagram** است: `udp`، `auto` و `dgtun`، که از مسیر `udpcarrier/listen.go:184` و `udpcarrier/dial.go:214` آن را صدا می‌زنند. حامل قدیمی `noise` هم از آن استفاده می‌کند. مسیر جریانیِ TLS از `core` فقط ثابت‌های نوع قاب (`core.TypeData`، `core.TypePing`) و `core.KeepalivePad()` را برمی‌دارد (`engine/l3_link.go:210,235`).
- `reality` و `noise` حامل‌های قدیمی و آزمایشیِ «موتور بسته‌ای» (`engine/engine.go`) هستند و نصاب آن‌ها را پیشنهاد نمی‌دهد (`BUILD.md:50, 155-156`). فقط از مسیر `cmd/hs2/main.go:406-407, 414-415` و `runReality`/`runNoise` قابل اجرا هستند.

---

## ۲. اجزای اصلی

### ۲-۱. `tlscarrier` (حامل اصلی TLS)

| جزء | path:line | کار |
|---|---|---|
| `Carrier` | `tlscarrier/carrier.go:19-23` | یک نشست TLS احرازشده (`conn`: سمت سرور `*tls.Conn`، سمت کلاینت `*utls.UConn`)، به‌اضافهٔ `sendMu` و بافر خواندنِ قابل‌استفادهٔ مجدد `rbuf`. |
| `AppendFrame` / `WriteRaw` | `carrier.go:34-47` | قاب‌های بدون پرکننده را پشت هم در یک بافر می‌چیند و با یک `Write` می‌فرستد (مهلت نوشتن 5 ثانیه). |
| `SendFrame` / `SendFramePadded` / `writeFrame` | `carrier.go:52-79` | قاب `[ftype:1][reallen:3][padlen:3][payload][pad]`. در کد تولیدی صدا زده نمی‌شوند و فقط آزمون‌ها از آن‌ها استفاده می‌کنند. |
| `ReadFrame` / `ReadFrameReuse` / `readFrame` | `carrier.go:81-115` | خواندن قاب. طول واقعی و پرکننده هر کدام حداکثر 1MiB هستند (`carrier.go:103`). |
| `RawConn` | `carrier.go:122` | اتصال TLS خام را برای smux برمی‌گرداند. مسیر جریانی همیشه از همین استفاده می‌کند. |
| `TCPConn` | `carrier.go:127-147` | زنجیرهٔ پوشش‌ها را تا 8 لایه باز می‌کند تا به `*net.TCPConn` برسد؛ برای خواندن `TCP_INFO` (بازارسال‌ها و rwnd) لازم است. |
| `Dial` / `DialFrom` / `DialFromTimeout` | `carrier.go:154-217` | شماره‌گیری TCP (پیش‌فرض 8 ثانیه، و 2 ثانیه برای «پیشاهنگ» در حالت معکوس: `cmd/hs2/main.go:534-536`)، keepalive هر 3 ثانیه، `tuneTCP`، دست‌دهی utls Chrome 133، استخراج EKM، فرستادن رکورد احراز، و خواندن و وارسی اثبات سرور. |
| `clientEKM` | `carrier.go:223-231` | اگر نسخهٔ مذاکره‌شده TLS 1.3 نباشد خطا می‌دهد. پس از تأیید 1.3، `Renegotiation=RenegotiateNever` می‌گذارد، چون پیش‌تنظیمِ Chrome پشتیبانی از renegotiation را اعلام می‌کند و utls در آن حالت EKM را صادر نمی‌کند. |
| `Server` | `tlscarrier/server.go:16-30` | `SharedKey`، `Cert`/`GetCertificate` (تعویض داغ گواهی)، `BackendAddr` (پشتیبان پوششی)، `Logf`، حافظهٔ replay و محدودکنندهٔ لاگ. |
| `Server.Handle` | `server.go:72-148` | دست‌دهی TLS، تشخیص HTTP خام، استخراج EKM، خواندن «یک رکورد»، وارسی احراز، و بعد یا تحویل `Carrier` به `onTunnel` یا ارسال به پشتیبان. |
| `tlsRecordHeaderLooksLikeHTTP` | `server.go:61-67` | فقط پنج شروع دقیقی را که `net/http` می‌شناسد قبول می‌کند: `"GET /"`، `"HEAD "`، `"POST "`، `"PUT /"`، `"OPTIO"`. |
| `Server.forward` | `server.go:164-183` | جریان رمزگشایی‌شده را به پشتیبان می‌دهد (اتصال TCP به آن با مهلت 8 ثانیه)، بایت‌هایی را که قبلاً خوانده شده اول می‌فرستد، و دو goroutine برای `io.Copy` در دو جهت راه می‌اندازد. |
| `Server.logRare` | `server.go:151-160` | هر 30 ثانیه حداکثر یک لاگ. |
| `auth.go` | `tlscarrier/auth.go:53-203` | `exportEKM`، `minuteBucket`، `mac` (HMAC-BLAKE2s-256 بریده‌شده به 16 بایت)، `clientTag`، `serverTag`، `padded`، `makeClientAuth`، `parseClientAuth`، `isLegacyAuth`، `makeServerProof`، `readServerProof`. |
| `replayMem` | `tlscarrier/replaymem.go:9-59` | نقشهٔ nonceهای دیده‌شده (کلید 64 بیتی = XOR دو نیمهٔ nonce)، به‌اضافهٔ صف FIFO به ترتیب ورود. انقضا و حذف از ابتدای صف هزینهٔ سرشکنِ O(1) دارد. |
| `tuneTCP` و متغیرهای سراسری | `tlscarrier/tune_linux.go:11-51`، `tune_other.go:7-18` | `NotSentLowat`، `UserTimeoutMs`، `CongestionControl`. خطاهای `setsockopt` بی‌صدا نادیده گرفته می‌شوند. |

**goroutineها در tlscarrier:** خودِ `Handle` در goroutineای اجرا می‌شود که فراخواننده برای هر اتصالِ پذیرفته‌شده می‌سازد (`engine/stream_kharej.go:126`، `engine/stream_reverse.go:64`). `onTunnel` در همان goroutine اجرا می‌شود و تا پایان عمر لینک بلوکه می‌ماند. `forward` دو goroutine کپی می‌سازد (`server.go:177-178`).

### ۲-۲. شکل‌دهی طول (`engine/shape.go` و `obfs`)

| جزء | path:line | کار |
|---|---|---|
| `shapedConn` | `engine/shape.go:33-44` | پوششی دور `net.Conn` با `sampler`، `wbuf`، `rbuf`، `rem` و `hdr`. نوشتن و خواندن هر کدام mutex جدا دارند. |
| `newShapedConn` | `shape.go:53-58` | اگر نمونه‌گیر nil باشد، `obfs.NewHTTPSLengthSampler()` می‌سازد. |
| `shapedConn.Write` | `shape.go:63-103` | برای هر تکه یک `target` از توزیع می‌کشد. داده تا `target-4` بایت پر می‌شود و باقیِ آن (در تکهٔ آخر یا نوشتنِ کوچک) با صفر پر می‌شود. هر قاب با یک `Conn.Write` نوشته می‌شود، پس یک رکورد TLS می‌شود. |
| `shapedConn.Read` | `shape.go:107-132` | سرآیند 4 بایتی را می‌خواند. اگر `dataLen+padLen>32KiB` باشد `ErrUnexpectedEOF` برمی‌گرداند. پرکننده دور ریخته می‌شود. |
| `newSession` | `engine/stream.go:172-203` | **تنها نقطه‌ای** که شکل‌دهی اعمال می‌شود. ترتیب لایه‌ها: smux روی `watchConn`، روی `meteredConn` (اختیاری)، روی `shapedConn`، روی `conn`. |
| `obfs.LengthSampler` | `obfs/shaper.go:24-69` | توزیع گسستهٔ ثابت (جدول بخش ۴). `Sample()` با `crypto/rand` کار می‌کند. فیلدهای `mu` و `seeded` استفاده نمی‌شوند. |
| `obfs.Pacer`، `NextDelay`، `jitter`، `UDPHeaderPrefix` | `obfs/shaper.go:71-135` | **کد مرده:** در هیچ جای کد تولیدی صدا زده نمی‌شوند (با grep بررسی شد). |
| `obfs/dpi_eval.py`، `obfs/dpi_eval2.py` | — | شبیه‌سازهای ردهبند «نقش DPI» با رگرسیون لجستیک و AUC. فقط مستند و استدلال هستند و در ساخت نقشی ندارند. |
| `obfs/design.md` | — | فقط کلمهٔ `placeholder` در آن است. |

### ۲-۳. `core` (هستهٔ رمزنگاری Noise برای datagram/noise)

| جزء | path:line | کار |
|---|---|---|
| دست‌دهی | `core/handshake.go:13-295` | الگوی `Noise_IKpsk2_25519_ChaChaPoly_BLAKE2s`. prologue شامل برچسب `hs2-IKpsk2-v1` و سطل زمانی 30 ثانیه‌ای است. پیش از هر کار Noise یک `firstMAC` 16 بایتی (کلیددار با psk، روی سطل و کلید ephemeral) وارسی می‌شود. حافظهٔ replay وجود دارد، و بیت بالای ephemeral روی سیم تصادفی می‌شود. |
| `Initiator` / `Responder` | `handshake.go:107-245` | `WriteMessage1Payload`، `ReadMessage1Payload` (سطل‌های now، now-1 و now+1)، `WriteMessage2Payload`، `ReadMessage2Payload` (در `core/datagram.go:46-53`). |
| `foldSecret` / `csFingerprint` | `handshake.go:250-275` | دو CipherState را به یک راز 32 بایتی تبدیل می‌کند: BLAKE2s روی «رمزشدهٔ یک بلوک صفر در nonce=0». ساختاری غیراستاندارد است. |
| `Session` | `core/session.go:23-229` | از راز، با هش جداسازی‌شده بر اساس برچسب، چهار زیرکلید می‌سازد: AEAD i→r و r→i، و کلید ماسک طول i→r و r→i. به‌اضافهٔ `exporter`. AEAD از نوع ChaCha20-Poly1305 است. |
| قاب | `core/frame.go:15-136` (فایل از خط 1) | `[len:2 ماسک‌شده با ChaCha20(lenKey, seq)][AEAD(type,flags,plen,seq,payload,pad)+tag16]`. |
| پنجرهٔ replay | `core/replay.go:1-84` | بیت‌نقشهٔ 2048 تایی به سبک WireGuard/RFC 6479 با آدرس‌دهیِ `seq mod 2048`. |
| حافظهٔ replay دست‌دهی | `core/replaymem.go:1-47` | نقشهٔ MACها؛ TTL برابر 180 ثانیه؛ پاک‌سازی فقط وقتی اندازه از 8192 بگذرد. |
| شکل‌دهی core | `core/shape.go:1-107` | `PadTarget` (سطل‌ها برای noise)، `PadDatagramTarget` (برای udpcarrier)، `HandshakePad`، `KeepaliveJitter`، `KeepalivePad`. |
| پشتیبانی datagram | `core/datagram.go:1-99` | نوع‌های قاب 8 تا 15، `Exporter`، `Binding`، `HandshakeBinding`، `PeerStatic`، `StaticFromSeed` (کلیدهای ایستا را به‌طور قطعی از کلید مشترک مشتق می‌کند). |

### ۲-۴. `reality` و `noise` (برای مقایسه)

| جزء | path:line | کار |
|---|---|---|
| `reality.Dispatcher.Handle` | `reality/server.go:22-69` | یک رکورد ClientHello را با مهلت 5 ثانیه می‌خواند (بافر 2048 بایت). اگر session-id آن معتبر باشد اتصال را به `OnTunnel` می‌دهد، وگرنه کل اتصال را به سایت پوششی (`CoverAddr`) می‌فرستد. |
| `makeSessionID` / `verifySessionID` | `reality/tls_signal.go:28-57` | session-id 32 بایتی به شکل `[nonce16][HMAC(key,"reality-sid-v1"‖client_random‖nonce)[:16]]`. |
| `ClientDialTLS` | `reality/carrier.go:73-99` | utls با **`HelloChrome_120`**: `BuildHandshakeState`، جایگزین‌کردن SessionId، سپس `MarshalClientHello` و `Handshake`. |
| `ServerTLS` | `reality/carrier.go:42-51` | `tls.Server` روی `prefixConn` که ClientHelloِ خوانده‌شده را دوباره به ماشین حالت TLS می‌دهد، با گواهی **محلی** (`cert_file`). |
| `relayProbe` / `pipe` | `reality/relay.go:36-73` | «مسیر 1»: رلهٔ کامل گواهیِ سایت واقعی. **هیچ فراخواننده‌ای ندارد.** |
| `authTag`، `verifyTag`، `clientHelloRecord`، `DialSignalled` | `reality/auth.go`، `carrier.go:61-67`، `client.go:21-63` | `authTag`/`verifyTag` و `clientHelloRecord` استفاده نمی‌شوند. `DialSignalled` فقط در آزمون‌ها به کار می‌رود. |
| `realityCarrier` | `engine/carrier_reality.go:23-70` | قاب `[ftype:1][len:3][payload]` داخل TLS، بدون پرکننده. |
| `noiseCarrier` | `engine/carrier_noise.go:16-91` | `core.Session.Seal` با `PadTarget`. خواندن با `deadAfter`=15s. |
| `dialCarrier` / `acceptCarrier` / `writeBlock` / `readBlock` | `engine/carrier_tcp.go:24-134` | دست‌دهی Noise روی TCP با قاب‌بندی `[outer:2][inner:2][msg][pad 16..255]`. |

### ۲-۵. وصل‌کننده‌ها در `cmd/hs2`

| جزء | path:line | کار |
|---|---|---|
| `certReloader` | `cmd/hs2/cert.go:19-156` | با `atomic.Pointer[tls.Certificate]` گواهی را نگه می‌دارد. با SIGHUP بازخوانی می‌شود (`main.go:391-402`) و هر یک دقیقه mtime فایل پایش می‌شود (`cert.go:118-142`). اگر گواهی تازه خراب باشد، گواهی قدیمی می‌ماند. |
| `streamBackend` / `startBuiltinBackend` | `cmd/hs2/main.go:781-789, 945-993` | اگر `backend_addr` خالی یا `"builtin"` باشد، یک سرور HTTP روی `127.0.0.1:0` بالا می‌آید با صفحهٔ پوششیِ مخصوص هر نصب. |
| `buildCover` | `cmd/hs2/cover.go:250-449` | صفحه‌ای قطعی که فقط تابعی از (`cover_seed`، سال) است، با جریان شمارنده‌ای از SHA-256. اگر seed خالی باشد، همان صفحهٔ قدیمی و ثابت برگردانده می‌شود (`cover.go:251-253`). |
| `applyTuning` / `runCmd` | `cmd/hs2/main.go:258-275, 351-359` | متغیرهای محیطی `HS2_TUNE_*`. الگوریتم کنترل ازدحام از طرح `tune` می‌آید (`tune/tune.go:299-306`: پیش‌فرض bbr، و اگر نبود cubic). |

---

## ۳. جریان داده و کنترل، گام‌به‌گام

### ۳-۱. برقراری یک لینک mtcp/l3mtcp (حالت مستقیم: ایران شماره می‌گیرد، خارج گوش می‌دهد)

1. **دروازهٔ شماره‌گیری:** هر شماره‌گیری لینک در کل فرایند از یک دروازه نوبت می‌گیرد: حداکثر 8 دست‌دهیِ هم‌زمان، و شروع‌ها 40 تا 160 میلی‌ثانیه از هم فاصله دارند (حدود 10 در ثانیه) (`engine/dialgate.go:9-25`، `engine/linkmanager.go:394-396`). دلیل نوشته‌شده: انبوهی از ClientHelloهای یکسان از یک IP به یک IP:port «کاری است که هیچ مرورگری نمی‌کند».
2. `mtcpDialer.DialLink` تابع `tlscarrier.DialFrom(addr, sni, key, bindIP)` را صدا می‌زند (`engine/mtcp_link.go:286-292`).
3. اتصال TCP با مهلت 8 ثانیه برقرار می‌شود. اگر `bind_local_ip` نامعتبر باشد خطا برمی‌گردد و هرگز به IP پیش‌فرض برنمی‌گردد (`carrier.go:170-178`). سپس `SetKeepAlive(true)` و `SetKeepAlivePeriod(3s)` و `tuneTCP` (`carrier.go:186-190`).
4. `utls.UClient(raw, {ServerName: sni, InsecureSkipVerify: true}, HelloChrome_133)`. مهلت کل عملیات (دست‌دهی، احراز و اثبات) 10 ثانیه است (`authTimeout`، `carrier.go:193`).
5. **سمت سرور** (`server.go:72-111`): `tuneTCP(raw)` اجرا می‌شود و `tls.Config{MinVersion: TLS1.2, NextProtos: ["http/1.1"], GetCertificate}` ساخته می‌شود. دست‌دهی 10 ثانیه مهلت دارد. اگر شکست بخورد:
   - اگر خطا `tls.RecordHeaderError` باشد و 5 بایت اول یکی از پنج شروعِ HTTP باشد، پاسخ دقیق Go یعنی `httpToHTTPS400` فرستاده و اتصال بسته می‌شود (`server.go:94-99`).
   - در غیر این صورت `raw.Close()` (`server.go:100`).
6. هر دو طرف EKM را صادر می‌کنند: `ExportKeyingMaterial("EXPORTER-hs2-channel-binding-v2", nil, 32)` (`auth.go:87-89`). کلاینت روی TLS 1.3 پافشاری می‌کند (`carrier.go:223-231`). در سرور، اگر صدور EKM شکست بخورد، `raw.Close()` بدون هیچ پاسخی اجرا می‌شود (`server.go:103-108`؛ مشاهدهٔ م4 را ببینید).
7. **کلاینت** رکورد احراز را با یک `Write` می‌فرستد که یک رکورد TLS می‌شود: `nonce16 ‖ tag16 ‖ padlen16 ‖ pad(280..720)` با `tag = HMAC-BLAKE2s(key, "hs2-auth-v2"‖nonce‖be64(minute)‖EKM)[:16]` (`auth.go:131-136`؛ `carrier.go:206-210`).
8. **سرور** با مهلت 30 ثانیه (`firstReadTimeout`) دقیقاً **یک** `Read` روی بافر 16KiB انجام می‌دهد، یعنی یک رکورد (`server.go:114-121`). اگر هیچ بایتی نیاید (`n==0`)، `tconn.Close()`.
9. `parseClientAuth` (`auth.go:140-159`): طول باید دست‌کم 34 بایت باشد. برچسب برای 5 سطل دقیقه‌ای (−2 تا +2) با مقایسهٔ زمان‌ثابت وارسی می‌شود. سپس `more = 34 + padlen - len(first)`؛ اگر منفی باشد یعنی بایت اضافی آمده و «قاب ما نیست».
   - **اگر نامعتبر باشد:** اگر رکورد، رکورد احراز v1 باشد، یک لاگ نادر نوشته می‌شود. سپس مهلت برداشته می‌شود و `forward` بایت‌های خوانده‌شده را به پشتیبان می‌دهد (`server.go:123-129`).
   - **اگر معتبر باشد:** مهلت 10 ثانیه گذاشته می‌شود و `more` بایتِ باقی‌ماندهٔ پرکننده خوانده و دور ریخته می‌شود. سپس `replay.add(nonce)` اجرا می‌شود؛ اگر nonce تکراری باشد اتصال بسته می‌شود (`server.go:131-141`).
10. سرور اثبات را می‌نویسد: `serverTag16 ‖ padlen16 ‖ pad(120..480)` با `serverTag = HMAC(key, "hs2-srv-v2"‖nonce‖EKM)[:16]` (`auth.go:182-184`؛ `server.go:142-145`). سپس مهلت برداشته می‌شود و `onTunnel(&Carrier{conn: tconn})` صدا زده می‌شود.
11. **کلاینت** با `readServerProof` (`auth.go:187-203`) 18 بایت می‌خواند:
    - اگر با `"HTTP/"` شروع شود، یعنی سرور مثل وب‌سایت پاسخ داده و `ErrOldServer` برمی‌گردد (کلید متفاوت، یا hs2 قدیمی‌تر از v2، یا طبق مشاهدهٔ م9 اختلاف ساعت).
    - اگر برچسب نادرست باشد، `ErrServerProof`.
    - در غیر این صورت پرکننده دور ریخته می‌شود و مهلت پاک می‌شود. **کلاینت تا اثبات سرور را نبیند، هیچ دادهٔ تونلی نمی‌فرستد.**
12. لبه: `newEdgeLink(car, sampler)` و بعد `newSession(car.RawConn(), false, sampler, mtr)` که کلاینت smux است (`mtcp_link.go:299-307`). خروجی: `newSession(car.RawConn(), true, nil, mtr)` که سرور smux است (`engine/stream_kharej.go:126-155`). از این‌جا همهٔ بایت‌ها از `shapedConn` می‌گذرند.

حالت معکوس (`reverse`) همین مسیر است با این تفاوت که خارج شماره می‌گیرد (`RevDial`/`RevDialScout`، `main.go:531-536`) و ایران `tlscarrier.Server` را اجرا می‌کند (`main.go:492-498`، `engine/stream_reverse.go:47-101`). لبهٔ معکوس حداکثر `2*max_links+8` لینک نگه می‌دارد و لینکِ ردشده را 5 ثانیه نگه می‌دارد و بعد می‌بندد (`stream_reverse.go:35-41, 68-83`).

### ۳-۲. مسیر داده پس از برقراری (نوشتن)

1. یک جریان smux (مثلاً `kindL3` با قاب‌های `AppendFrame(TypeData, ipPacket)` که تا 16KiB در یک دسته جمع شده‌اند، `engine/l3_link.go:202-243`) روی جریان می‌نویسد.
2. `sendLoop` در smux هر قاب را (سرآیند 8 بایتی و دادهٔ حداکثر 16KiB) با **یک** `Write` به `watchConn` می‌دهد. `watchConn` متد `WriteBuffers` ندارد، پس smux از مسیر کپی استفاده می‌کند (`smux@v1.5.24/session.go:471-498`).
3. `meteredConn` می‌شمارد و `shapedConn.Write` قاب را به تکه‌هایی با طول `target ∈ {40,80,150,250,400,600,900,1200,1400}` می‌شکند. هر تکه یک `tls.Conn.Write` است، پس یک رکورد TLS با طول روی سیم `target+22` (5 سرآیند، 1 نوع محتوا و 16 برچسب AEAD).
4. سوکت TCP با `TCP_NOTSENT_LOWAT=32KiB` زود بلوکه می‌شود، و فشار برگشتی به smux و بعد به جریان‌ها می‌رسد.
5. در مسیر smux روی `RawConn` هیچ مهلت نوشتنی از tlscarrier اعمال نمی‌شود (مهلت 5 ثانیه فقط مال `WriteRaw`/`SendFrame` است). تشخیص لینکِ گیرکرده به `TCP_USER_TIMEOUT` (20 ثانیه)، keepalive در smux (مهلت 24 ثانیه) و نگهبان wedge (`engine/wedge.go`) سپرده شده است.

### ۳-۳. مسیر کاوشگر و مرورگر (سرور)

| ورودی کاوشگر | رفتار | محل |
|---|---|---|
| HTTP خام که با یکی از پنج شروع Go آغاز شود | `HTTP/1.0 400 Bad Request…Client sent an HTTP request to an HTTPS server.\n`، سپس بستن با همان FIN یا RSTی که Go می‌دهد | `server.go:94-99`؛ آزمون‌های `TestPlainHTTPGetsGoNative400` و `TestProbeCloseMatchesStdlib` |
| هر ورودیِ دیگرِ غیر TLS (DELETE، پیش‌درآمد h2c، بایت تصادفی، سرآیند TLS خراب) | بستن بدون هیچ بایتی؛ FIN یا RST دقیقاً مثل Go | `server.go:100`؛ `TestUnrecognizedMethodClosed`، `TestGarbageClosed` |
| دست‌دهی TLS که در 10 ثانیه تمام نشود | بستن | `server.go:86` |
| TLS کامل بدون هیچ درخواستی | بعد از 30 ثانیه `tconn.Close()` (همراه با close_notify) | `server.go:114-120` |
| TLS کامل و درخواست HTTP | ارسال به پشتیبان؛ برای `/` صفحهٔ پوششی، برای بقیه 404 معمولی Go | `server.go:128`؛ `main.go:972-989` |
| TLS کامل و بایت تصادفی، یا رکورد احراز با کلید یا ساعت غلط | ارسال به پشتیبان، که سرور HTTP آن جواب 400 می‌دهد | `server.go:123-129` |
| رکورد احراز معتبرِ ضبط‌شده که روی اتصال دیگری بازپخش شود | EKM جور نیست، پس به پشتیبان می‌رود | `TestReplayRejected` |
| کاوشگر TLS 1.2 **بدون** Extended Master Secret | صدور EKM خطا می‌دهد، پس `raw.Close()` بلافاصله پس از دست‌دهی و بدون پاسخ | `server.go:103-108`؛ برای Go 1.27 در `crypto/tls/conn.go` (`noEKMBecauseNoEMS`) بررسی شد؛ مشاهدهٔ م4 |
| SNI غلط یا بدون SNI | همان گواهی و همان رفتار | `CHANGELOG.md:147` |

### ۳-۴. گواهی

- راه‌اندازی: `newCertReloader(cert_file, key_file)`؛ اگر بارگذاری اول شکست بخورد، `must` فرایند را متوقف می‌کند (`cert.go:35-44`؛ `main.go:541-542`).
- در هر دست‌دهی، `GetCertificate` اشاره‌گر فعلی را برمی‌گرداند (`cert.go:68-70`). اتصال‌های موجود دست نمی‌خورند.
- **کلاینت گواهی را اعتبارسنجی نمی‌کند** (`InsecureSkipVerify: true`، `carrier.go:191, 198-200`). اعتماد فقط از اتصال به EKM می‌آید. پیامد ثبت‌شده: گواهی DNS-01 که خودکار تمدید نشده بود منقضی شد و تونل همچنان کار کرد، ولی هر کاوشگری یک گواهی منقضی می‌دید (`CHANGELOG.md:247-254`؛ `README.md:279-282`).
- نصاب گواهی Let's Encrypt را با HTTP-01 یا DNS-01 می‌گیرد یا مسیر گواهیِ موجود را می‌پذیرد. گزینهٔ «خودامضا» ندارد (`install/install.sh:1360-1384`). کلید مشترک با `openssl rand -hex 32` ساخته می‌شود (32 بایت، `install.sh:1851`).

### ۳-۵. دست‌دهی Noise (مسیر `noise` و، با `StaticFromSeed`، udpcarrier)

1. آغازگر: `NewInitiator` با prologueِ سطل جاری ساخته می‌شود. `WriteMessage1Payload` بیت بالای ephemeral را تصادفی می‌کند و `firstMAC(psk, bucket, ephemeral)` را جلوی پیام می‌گذارد (`handshake.go:144-153`).
2. پاسخ‌دهنده (`handshake.go:186-231`): طول باید دست‌کم 48 باشد. MAC برای سطل‌های now، now-1 و now+1 وارسی می‌شود؛ سپس `withinWindow` (مشاهدهٔ م19)، سپس `seen.add(mac)` برای ضد replay، سپس پاک‌کردن بیت بالا، و آخر `ReadMessage` در Noise. هر شکستی پیش از Noise به `ErrHandshakeAuth`، `ErrHandshakeStale` یا `ErrHandshakeReplay` ختم می‌شود.
3. `WriteMessage2Payload` و `foldSecret`، و بعد `NewSession(secret, initiator, id)`.
4. در حامل `noise` روی TCP، پیام‌ها با `writeBlock` و `HandshakePad` (16 تا 255 بایت تصادفی) قاب‌بندی می‌شوند (`engine/carrier_tcp.go:100-114`). اگر پیام اول رد شود، اتصال **فقط بسته می‌شود**؛ «فریب» (decoy) هرگز پیاده نشد (`carrier_tcp.go:69-71`، `engine/carrier_noise.go:84-87`).

### ۳-۶. reality (آزمایشی)

کلاینت یک ClientHelloِ Chrome 120 با session-id امضاشده می‌فرستد. Dispatcher یک رکورد را می‌خواند و session-id را وارسی می‌کند. اگر معتبر نباشد اتصال به `cover_addr` (سایت واقعی) فرستاده می‌شود. اگر معتبر باشد، `ServerTLS` دست‌دهی را با گواهی **محلی** کامل می‌کند و قاب‌های hs2 داخل TLS جریان می‌یابند. رلهٔ دست‌دهیِ سایت واقعی («مسیر 1»، `relay.go`) نوشته شده ولی سیم‌کشی نشده است.

---

## ۴. جدول ثابت‌ها، آستانه‌ها، بافرها و زمان‌سنج‌ها

### ۴-۱. tlscarrier

| نام | مقدار | path:line | معنی |
|---|---|---|---|
| `writeTimeout` | 5s | `tlscarrier/carrier.go:27` | مهلت هر `WriteRaw`/`SendFrame` (مسیر smux از آن استفاده نمی‌کند) |
| مهلت اتصال `DialFrom` | 8s | `carrier.go:162` | اتصال TCP لینک |
| مهلت اتصال پیشاهنگ معکوس | 2s | `cmd/hs2/main.go:535` | `RevDialScout` |
| دورهٔ keepalive هسته در کلاینت | 3s | `carrier.go:188` | تشخیص مسیرِ سیاه‌چاله‌شده (سمت سرور این مقدار را نمی‌گذارد) |
| `authTimeout` | 10s | `tlscarrier/auth.go:59` | در کلاینت کل دست‌دهی، احراز و اثبات (`carrier.go:193`)؛ در سرور خواندن باقیِ رکورد احراز و نوشتن اثبات (`server.go:131`) |
| `handshakeTimeout` | 10s | `tlscarrier/server.go:47` | سقف دست‌دهی TLS در سرور |
| `firstReadTimeout` | 30s | `server.go:40` | سکوت مجاز پس از دست‌دهی (عمداً کمتر از 60 ثانیهٔ nginx) |
| بافر خواندن اول | 16KiB | `server.go:115` | یک رکورد TLS |
| مهلت اتصال به پشتیبان | 8s | `server.go:166` | `forward` |
| فاصلهٔ `logRare` | 30s | `server.go:156` | محدودکنندهٔ لاگ |
| `authNonceLen` / `authTagLen` | 16 / 16 | `auth.go:54-55` | — |
| `authHeadLen` / `proofHeadLen` | 34 / 18 | `auth.go:56-57` | — |
| `authClockSkew` | ±2 سطل دقیقه‌ای | `auth.go:58`، `auth.go:91` | رواداری مؤثر بین 2 و 3 دقیقه، بسته به جایگاه در دقیقه |
| `exporterLabel` | `EXPORTER-hs2-channel-binding-v2`، طول 32 | `auth.go:60, 88` | برچسب EKM |
| پرکنندهٔ کلاینت | 280–720 بایت | `auth.go:62` | «اندازهٔ یک GET در HTTP/1.1»؛ رکورد روی سیم 336–776 بایت |
| پرکنندهٔ سرور | 120–480 بایت | `auth.go:63` | «اندازهٔ سرآیندهای پاسخ»؛ رکورد روی سیم 160–520 بایت |
| `legacyRecordLen` | 32 | `auth.go:65` | رکورد احراز v1 (رد می‌شود و فقط لاگ می‌گیرد) |
| `replayTTL` / `replayCap` | 5min / 16384 | `tlscarrier/replaymem.go:16-17` | حافظهٔ nonce |
| آستانهٔ فشرده‌سازی FIFO | `head>1024 && head*2>len` | `replaymem.go:49` | — |
| `NotSentLowat` | 32KiB | `tlscarrier/tune_linux.go:19` | بر پایهٔ آزمایشگاه: 16 تا 32KiB بهترین بود، 64KiB و بیشتر تأخیر افزود، و خاموش‌کردنش 3 تا 7 برابر بدتر بود |
| `UserTimeoutMs` | 20000 | `tune_linux.go:22` | `TCP_USER_TIMEOUT` |
| `CongestionControl` | `"bbr"` (و پس از طرح tune، bbr یا cubic) | `tune_linux.go:29`؛ `main.go:357-359`؛ `tune/tune.go:299-306` | — |
| سقف طول قاب `Carrier` | واقعی و پرکننده هر کدام ≤1MiB | `carrier.go:103` | — |
| سقف قاب L3 روی جریان | ≤64KiB | `engine/stream.go:227` | `streamPkt` |
| حداکثر لایه‌های بازشده در `TCPConn` | 8 | `carrier.go:136` | محافظ در برابر حلقه |

### ۴-۲. شکل‌دهی و smux

| نام | مقدار | path:line | معنی |
|---|---|---|---|
| توزیع `NewHTTPSLengthSampler` | 1400:0.55، 1200:0.08، 900:0.05، 600:0.05، 400:0.05، 250:0.06، 150:0.06، 80:0.06، 40:0.04 | `obfs/shaper.go:41-42` | 9 اندازهٔ گسسته؛ میانگین هدف حدود 991 بایت؛ رکورد روی سیم = هدف+22 (62 تا 1422) |
| `shapeHdrLen` | 4 | `engine/shape.go:47` | `[dataLen:u16][padLen:u16]` |
| `shapeMaxFrame` | 32KiB | `shape.go:50` | محافظ گیرنده |
| سربار اندازه‌گیری‌شده در آزمون | 0.68٪ برای انتقال انبوه 256KiB؛ 287 قاب؛ 9 اندازهٔ متمایز | `engine/shape_test.go:100` (اجرا شد) | سقف آزمون 15٪ |
| `SmuxFrameSize` | 16KiB | `engine/mtcp_link.go:258` | — |
| `SmuxStreamBuffer` | 2MiB | `mtcp_link.go:262` | قاب UPD تقریباً هر 1MiB مصرف‌شده |
| `SmuxSessionBuffer` | 8MiB | `mtcp_link.go:264` | — |
| نسخهٔ smux | 2 | `mtcp_link.go:269` | — |
| `KeepAliveInterval` | 4000+rand(4000) میلی‌ثانیه، یک بار برای هر نشست | `mtcp_link.go:278` | ضد «ضربان ثابت 5 ثانیه‌ای» |
| `KeepAliveTimeout` | 24s | `mtcp_link.go:279` | — |
| `gateInflight` | 8 | `engine/dialgate.go:21` | دست‌دهی‌های هم‌زمان |
| `jitterGap` | 40–160ms | `engine/linkmanager.go:394-396` | فاصلهٔ شروع دست‌دهی‌ها (حدود 10 در ثانیه) |
| `reverseAcceptSlack` / `reverseRefuseHold` | 8 / 5s | `engine/stream_reverse.go:36-37` | لبهٔ معکوس |

### ۴-۳. پوشش و گواهی

| نام | مقدار | path:line | معنی |
|---|---|---|---|
| پایش گواهی | هر 1 دقیقه | `cmd/hs2/cert.go:119` | تغییر mtime |
| هشدار انقضا | ≤7 روز، یک بار | `cert.go:136-138` | — |
| `ReadHeaderTimeout` پشتیبان داخلی | 10s | `cmd/hs2/main.go:990` | `IdleTimeout` و `ReadTimeout` تنظیم نشده‌اند |
| `Cache-Control` | `max-age=3600` | `main.go:981` | — |
| فاصلهٔ `Last-Modified` | 18–400 روز (صفحهٔ قدیمی: 37) | `cmd/hs2/cover.go:449, 252` | — |
| ETag | `"hex(sha256("hs2-cover-etag\0"+seed+"\0"+page)[:16])"`؛ صفحهٔ قدیمی ETag ندارد | `main.go:967-971` | — |
| `cover_seed` | 16 بایت تصادفی (32 کاراکتر hex) | `install/install.sh:1855` | مستقل از کلید |

### ۴-۴. core و حامل‌های قدیمی

| نام | مقدار | path:line | معنی |
|---|---|---|---|
| `hsPattern` | `hs2-IKpsk2-v1` | `core/handshake.go:32` | prologue |
| `tsWindow` | 90s | `handshake.go:33` | در عمل محدودکننده نیست (م19) |
| سطل زمانی | 30s | `handshake.go:74` | — |
| `firstMsgMinLen` | 32 | `handshake.go:34` | — |
| `ephemeralLen` | 32 | `handshake.go:98` | — |
| `replayMemTTL` / `replayMemCap` | 180s / 8192 | `core/replaymem.go:15-16` | — |
| `frameHeaderLen` / `tagLen` / `lenPrefix` / `maxPayload` | 12 / 16 / 2 / 60000 | `core/frame.go:25-28` | — |
| `replayBits` | 2048 | `core/replay.go:20` | — |
| `padBuckets` | 64، 128، 256، 512، 1024، 1400 | `core/shape.go:23, 25` | فقط برای noise |
| `dgPadBuckets` | 64، 128، 256، 512، 1024 (همیشه کمتر از MTU) | `core/shape.go:45` | udpcarrier (`udpcarrier/carrier.go:778`) |
| `HandshakePad` | 16–255 | `core/shape.go:85` | — |
| `KeepaliveJitter` | پایه ±40٪ | `core/shape.go:90-94` | موتور قدیمی: 5s ±40٪ (`engine/engine.go:29, 157-169`) |
| `KeepalivePad` | 0–95 | `core/shape.go:100` | — |
| `deadAfter` موتور قدیمی | 15s | `engine/engine.go:30` | — |
| مهلت Noise روی TCP | شماره‌گیری 8s، خواندن m1 10s | `engine/carrier_tcp.go:25, 73` | — |
| `peekWait` در reality | 5s | `reality/server.go:35` | — |
| بافر ClientHello در reality | 2048 | `reality/server.go:48` | — |
| مهلت شماره‌گیری پوشش و dial در reality | 8s | `reality/server.go:97`؛ `tls_signal.go:60` | — |

---

## ۵. حلقه‌های کنترلی

| حلقه | ورودی | شرط | خروجی | دوره |
|---|---|---|---|---|
| `watchCerts` | mtime فایل گواهی، زمان انقضا | mtime تغییر کرده، یا ≤7 روز مانده و هنوز هشدار نداده | `reload("file changed on disk")`، لاگ هشدار | هر 1 دقیقه (`cert.go:118-142`) |
| بازخوانی با SIGHUP | سیگنال | — | `reloadAllCerts("SIGHUP")` | رویدادمحور (`main.go:391-402`) |
| keepalive در smux | — | — | قاب NOP (که به اندازهٔ نمونه‌برداری‌شده پر می‌شود) | 4 تا 8 ثانیه برای هر نشست و هر طرف؛ مهلت 24s |
| keepalive هسته در کلاینت | سکوت | بیکاری ≥3s | کاوش‌های keepalive؛ همراه `TCP_USER_TIMEOUT` حدود 20s | 3s |
| `TCP_USER_TIMEOUT` | دادهٔ تأییدنشده | ≥20s | خطای سوکت، سپس `watchConn.fail`، سپس بستن نشست | پیوسته (هسته) |
| انقضای `replayMem` | هر `add` | قدیمی‌تر از 5 دقیقه یا اندازه ≥16384 | حذف از ابتدای FIFO | به ازای هر احراز (سرشکنِ O(1)) |
| `logRare` | لاگ کلاینت قدیمی | 30s گذشته باشد | یک خط لاگ | — |
| دروازهٔ شماره‌گیری | درخواست لینک | جای خالی (≤8) و زمان شروع رسیده | اجازهٔ دست‌دهی | فاصلهٔ 40 تا 160 میلی‌ثانیه |
| keepalive موتور قدیمی (reality/noise) | — | — | `TypePing` با پرکنندهٔ 0 تا 95 | 5s ±40٪ |
| keepalive پیوند L3 (روی جریان) | بیکاریِ صف | تایمر بیکاری | `AppendFrame(TypePing, KeepalivePad())` | `l3KeepaliveEvery`، یا در حالت «آرام» حول 10s (`engine/l3_link.go:79, 126-135`؛ جزئیات در زیرسیستم L3) |

---

## ۶. حالت‌ها، گذارها، خطاها و بازیابی

### ۶-۱. ماشین حالت `Server.Handle`

```
پذیرش → tuneTCP → دست‌دهی TLS (10s)
   ├─ RecordHeaderError + شروع HTTP → 400 دقیق Go → بستن
   ├─ هر خطای دیگر → raw.Close
   └─ موفق → صدور EKM
         ├─ خطا (TLS1.2 بدون EMS) → raw.Close
         └─ خواندن یک رکورد (30s)
               ├─ n==0 → tconn.Close
               ├─ احراز نامعتبر → (اگر v1 بود: logRare) → forward به پشتیبان
               └─ معتبر → دور ریختن پرکننده (10s)
                     ├─ nonce تکراری → بستن
                     ├─ نوشتن اثبات شکست خورد → بستن
                     └─ onTunnel(Carrier)
```

### ۶-۲. ماشین حالت کلاینت `DialFromTimeout`

```
ParseIP(bindIP)؟ ─خطا→ "tlscarrier: invalid bind_local_ip"
اتصال TCP ─خطا→ خطای شبکه
keepalive 3s، tuneTCP، مهلت 10s
دست‌دهی utls Chrome133 ─خطا→ بستن و خطا
clientEKM ─نه TLS1.3→ "server did not negotiate TLS 1.3"
نوشتن رکورد احراز ─خطا→ بستن
readServerProof ─ "HTTP/" → ErrOldServer
                ─ برچسب نادرست → ErrServerProof
                ─ کمبود داده → "tlscarrier: reading server proof: …"
پاک‌کردن مهلت → Carrier
```

بازیابی در خودِ این لایه نیست. لایه‌های بالاتر لینک را دوباره می‌سازند (مدیر لینک در لبه، و `exitPool` در خروجیِ معکوس). خطاهای سوکت در `watchConn` به واژه‌های قابل‌فهم برای اپراتور تبدیل می‌شوند (`engine/stream.go:136-161`).

### ۶-۳. core

پیام اول نامعتبر به `ErrHandshakeAuth`، `ErrHandshakeStale` یا `ErrHandshakeReplay` می‌رسد (`core/handshake.go:38-40`)؛ در حامل noise نتیجه بستن اتصال است. قاب خراب یا تزریق‌شده: AEAD شکست می‌خورد و یک خطای خواندن معمولی برمی‌گردد که حامل را می‌بندد. تضمین این است که «هیچ واکنش ویژه‌ای» وجود ندارد (`core/injection_test.go`). شمارهٔ ترتیبیِ تکراری یا خیلی قدیمی: `errReplayed` (`core/session.go:43`).

---

## ۷. قالب پیام‌ها و قاب‌ها

### ۷-۱. احراز tlscarrier (داخل TLS؛ نسخهٔ v2)

```
کلاینت → سرور (یک رکورد TLS):
  [nonce:16][tag:16][padlen:u16 BE][pad:padlen تصادفی، 280..720]
  tag = HMAC-BLAKE2s-256(key, "hs2-auth-v2" ‖ nonce ‖ be64(unix/60) ‖ EKM)[:16]

سرور → کلاینت (فقط پس از وارسی):
  [tag:16][padlen:u16 BE][pad:padlen تصادفی، 120..480]
  tag = HMAC-BLAKE2s-256(key, "hs2-srv-v2" ‖ nonce ‖ EKM)[:16]

EKM = ExportKeyingMaterial("EXPORTER-hs2-channel-binding-v2", nil, 32)

v1 (ردشده): [nonce:16][HMAC(key,"hs2-tls-auth"‖nonce‖be64(minute))[:16]]، دقیقاً 32 بایت
```
(`tlscarrier/auth.go:18-51, 107-113, 161-179`)

### ۷-۲. قاب `tlscarrier` (روی جریان `kindL3`)

```
[ftype:1][reallen:3 BE][padlen:3 BE][payload:reallen][pad:padlen صفر]
```
`AppendFrame` همیشه padlen=0 می‌گذارد (`carrier.go:34-38`). نوع‌ها از core می‌آیند: `TypeData=1`، `TypePing=3` (`core/frame.go:32-38`).

### ۷-۳. کدک `shapedConn` (زیر smux و داخل TLS)

```
[dataLen:u16 BE][padLen:u16 BE][data:dataLen][pad:padLen صفر]
طول کل = target (نمونه از LengthSampler)
```
(`engine/shape.go:25-32`)

### ۷-۴. سرآیند smux v2 (از کتابخانه)

```
[ver:1][cmd:1][len:u16 LE][sid:u32 LE][data]
```
(`smux@v1.5.24/session.go:487-490`). بایت اول هر جریان نوع آن است: 1=TCP، 2=UDP، 3=L3، 4=Ctrl، 5=Pool، 6=Stats، 7=Info، 8=TCPPort، 9=UDPPort (`engine/stream.go:33-48`).

### ۷-۵. core

```
قاب جریانی: [len:2 XOR ChaCha20(lenKey, nonce=seq)][AEAD_ChaCha20Poly1305(nonce=be64(seq) در 12 بایت،
             type:1 flags:1 plen:u16 seq:u64 payload pad)+tag:16]
قاب datagram: [seq:8][ciphertext همان قاب داخلی]   (core/session.go:163-215)
دست‌دهی: m1 = [firstMAC:16][Noise IK msg1 (e:32، s رمزشده:48، payload رمزشده:16+)]
         firstMAC = BLAKE2s-256(key=psk, "hs2 first mac v2" ‖ be64(bucket30s) ‖ ephemeral)[:16]
برچسب‌های اشتقاق: "hs2 traffic secret"، "hs2 i->r aead"، "hs2 r->i aead"، "hs2 i->r len"،
  "hs2 r->i len"، "hs2 exporter root"، "hs2 exporter v1"، "hs2 static from seed v1"
```

### ۷-۶. noise روی TCP

```
[outer:u16 BE][inner:u16 BE = len(msg)][msg][pad: 16..255 تصادفی]
```
(`engine/carrier_tcp.go:100-114`)؛ مشاهدهٔ م20 را ببینید.

### ۷-۷. reality

```
session-id (32): [nonce:16][HMAC-BLAKE2s(key, "reality-sid-v1" ‖ client_random ‖ nonce)[:16]]
قاب داخل TLS: [ftype:1][len:3 BE][payload]
```

---

## ۸. متن دقیق لاگ‌ها و خطاهای مهم

| متن | path:line | معنی |
|---|---|---|
| `tls: refused a pre-v2 client (no channel binding) from %s: upgrade the Iran server` | `tlscarrier/server.go:125` | کلاینت v1 (بدون اتصال به EKM) آمد و رد شد؛ هر 30 ثانیه حداکثر یک بار |
| `tlscarrier: server did not prove the shared key (wrong shared_key, man-in-the-middle, or not hs2)` | `tlscarrier/auth.go:71` | `ErrServerProof`: سرور برچسب درست نداد |
| `tlscarrier: the other server answered as a plain website, not as the tunnel: its shared_key differs from this one (paste the CURRENT link from it — running its setup again makes a new key), or it runs an hs2 older than v2 (upgrade it)` | `auth.go:78-80` | `ErrOldServer`: پاسخ با `HTTP/` شروع شد؛ پیام عمداً نام هیچ‌کدام از دو طرف را نمی‌آورد (`TestWrongServerKey`) |
| `tlscarrier: server did not negotiate TLS 1.3` | `tlscarrier/carrier.go:226` | — |
| `tlscarrier: reading server proof: %w` | `auth.go:190, 200` | اثبات نرسید یا ناقص بود (مهلت 10 ثانیه) |
| `tlscarrier: invalid bind_local_ip %q` | `carrier.go:175` | — |
| `cert: reload (%s) failed, keeping the current certificate: %v` | `cmd/hs2/cert.go:76` | — |
| `cert: reloaded (%s) — now valid until %s` / `cert: reloaded (%s) — unchanged` | `cert.go:80, 82` | — |
| `cert: %s expires in %d day(s) — renew it now; 'hs2 doctor' (cert renewal) shows whether and how it renews` | `cert.go:137` | — |
| `tuning: %s=%d` / `tuning: HS2_TUNE_CC=%q` | `cmd/hs2/main.go:263, 273` | بازنویسی آزمایشگاهی |
| `tuning: %s (not applied: %s)` | `main.go:355` | ریشه نیست یا `HS2_NO_TUNE` تنظیم شده |
| `link up from %s (now %d)` / `link down from %s: %s (now %d)` | `engine/stream_kharej.go:142, 154` | لینک در خروجیِ مستقیم |
| `mtcp: refused %d reverse link(s)%s (latest from %s): this server holds at most %d (twice its max_links %d + %d) — check the Kharej server's min_links/max_links` | `engine/stream_reverse.go:74-75` | — |
| `stream edge (reverse): listening for kharej links on %s` | `main.go:498` | — |
| `stream exit (reverse): dynamic link pool %d–%d to edge %s (edge drives the count)` | `main.go:537` | — |
| `"sni" is empty: the TLS handshake goes out without a domain name, which stands out to DPI` | `cmd/hs2/check.go:263` | هشدار `hs2 check` |
| `"shared_key" is %d bytes; the installer makes 32 (64 hex characters) — …` | `check.go:213` | — |
| `node time %s — both servers must agree within ~1 min (link auth is minute-bound)` | `cmd/hs2/doctor.go:77` (متن تقریبی؛ قالب کامل همان‌جاست) | اختلاف ساعت |
| `reality: authorised client (signalled ClientHello)` / `reality: cover dial failed: %v` | `reality/server.go:63, 99` | — |
| `core: handshake timestamp outside window` / `core: handshake first message replayed` / `core: handshake authentication failed` | `core/handshake.go:38-40` | — |

**نکته:** سرور در هیچ حالتی برای احرازِ شکست‌خورده لاگ نمی‌نویسد (جز کلاینت v1). کلید غلط یا اختلاف ساعت فقط در سمت کلاینت (`ErrOldServer`) دیده می‌شود.

---

## ۹. گزینه‌های پیکربندی و متغیرهای محیطی

| کلید یا متغیر | محل | اثر |
|---|---|---|
| `carrier` | `main.go:405-424` | `mtcp`، `l3mtcp`/`l3`، `tls` از `runStream` می‌روند؛ `reality` از `runReality`؛ `noise`/`""` از `runNoise` |
| `mode` (`dial`/`listen`)، `reverse` | `main.go:430, 468, 489, 520` | نقش لبه یا خروجی، و این‌که کدام طرف شماره می‌گیرد |
| `addr` | — | مقصد یا آدرس گوش‌دادن (پیش‌فرض نصاب: درگاه 2096؛ `install.sh:1632`) |
| `sni` | `main.go:89` | SNI در ClientHello؛ اگر خالی باشد `hs2 check` هشدار می‌دهد |
| `shared_key` | `main.go:93` | به hex؛ کلید HMAC احراز. `unhex` خطا را بی‌صدا نادیده می‌گیرد (`main.go:1005`) ولی `hs2 check` جلوی آن را می‌گیرد (`check.go:205-213`) |
| `cert_file` / `key_file` | `main.go:94-95` | گواهی طرفِ سرور TLS |
| `backend_addr` | `main.go:91` | پشتیبان کاوشگرها؛ `""` یا `"builtin"` یعنی صفحهٔ داخلی |
| `cover_seed` | `main.go:92` | seed صفحهٔ پوششیِ داخلی |
| `bind_local_ip` | `carrier.go:170-178`؛ `main.go:339-344` | IP مبدأ شماره‌گیری |
| `tuning.congestion` | `tune/tune.go:43, 299-306` | پیش‌فرض bbr |
| `cover_addr` (reality) | `main.go:90` | سایت واقعی برای کاوشگرها |
| `local_priv`، `local_pub`، `remote_static`، `psk` (noise) | `main.go:98-101` | — |
| `HS2_TUNE_NOTSENT` | `main.go:267` | `tlscarrier.NotSentLowat` |
| `HS2_TUNE_SMUX_FRAME` / `_STREAMBUF` / `_SESSBUF` | `main.go:268-270` | تنظیم smux |
| `HS2_TUNE_CC` | `main.go:271-274, 357` | بر طرح tune اولویت دارد |
| `HS2_NO_TUNE` | `main.go:349-356` | sysctlها اعمال نمی‌شوند |
| `godebug multipathtcp=0` | `go.mod:7`؛ `engine/listen.go:21-27, 41` | MPTCP خاموش است، چون سوکت پذیرفته‌شدهٔ MPTCP `tcp_notsent_lowat` را نادیده می‌گیرد |

---

## ۱۰. آزمون‌ها و آنچه تضمین می‌کنند

**tlscarrier** (`tlscarrier/carrier_test.go`، `server_probe_test.go`، `frame_test.go`، `replaymem_test.go`):
- `TestClientReachesTunnel`: کلاینت مجاز به تونل می‌رسد و قاب درست تحویل می‌شود.
- `TestProbeGetsBackend`: کاوشگری که با utls Chrome 133 دست می‌دهد گواهی دامنه را می‌بیند و پاسخ پشتیبان را می‌گیرد، نه تونل را.
- `TestReplayRejected`: رکورد احراز ضبط‌شده روی اتصال دیگر رد می‌شود، فقط یک تونل ساخته می‌شود، و بازپخش‌کننده به پشتیبان می‌رود.
- `TestMITMRejected`: میانجی با گواهی جعلی نه تونل می‌گیرد و نه کلاینت را فریب می‌دهد.
- `TestWrongServerKey`: کلید غلط شکست می‌خورد و پیام خطا نام `shared_key` را می‌آورد، نه «kharej» یا «iran».
- `TestShortProbeAnswered`: درخواست کوتاه‌تر از رکورد احراز فوراً از پشتیبان پاسخ می‌گیرد.
- `TestLegacyClientRefused`: رکورد v1 شناخته می‌شود ولی به‌عنوان v2 پذیرفته نمی‌شود.
- `TestPlainHTTPGetsGoNative400`: پاسخ بایت‌به‌بایت برابر `httpToHTTPS400` است.
- `TestPinHTTPSResponseMatchesStdlib`: ثابت با پاسخ واقعی `net/http` سنجاق شده است.
- `TestUnrecognizedMethodClosed`: DELETE هیچ پاسخی نمی‌گیرد.
- `TestServerCarrierTCPConn`: `TCPConn()` در هر دو سمت nil نیست.
- `TestProbeCloseMatchesStdlib`: ده کاوش به‌صورت تفاضلی با یک سرور HTTPS واقعی Go مقایسه می‌شوند، هم بایت‌ها و هم FIN یا RST. خروجی اجرا: GET و HEAD → `FIN`؛ GET با 3KB سرآیند → `RST`؛ DELETE، h2c، 100 بایت تصادفی، 3 بایت و FIN، سرآیند TLS با نسخهٔ بی‌معنی → `FIN`؛ 2000 بایت تصادفی → `RST`؛ رکورد دست‌دهی خراب → `FIN` همراه هشدار TLS.
- `TestGarbageClosed`: ورودی آشغال پاسخی نمی‌گیرد.
- `TestAppendFrameRoundTrip`: `AppendFrame`، `WriteRaw`، قاب پرشده، `ReadFrame` و `ReadFrameReuse` با هم سازگارند.
- `TestReplayMemBoundedAndRejectsReuse`: nonce تکراری رد می‌شود؛ نقشه هرگز از `replayCap` بزرگ‌تر نمی‌شود؛ 3×16384 افزودن در کمتر از 2 ثانیه انجام می‌شود؛ تازه‌ترین‌ها به خاطر می‌مانند.

**شکل‌دهی** (`engine/shape_test.go`):
- `TestShapedConnRoundTrip`: جریان بایت برای هر اندازهٔ نوشتن و خواندن دقیق می‌ماند.
- `TestShapedConnRecordSizes`: هیچ قابی از 1400 بزرگ‌تر نیست؛ دست‌کم 3 اندازهٔ متمایز وجود دارد؛ سربار انبوه کمتر از 15٪ است.
- `TestShapedConnPadsSmallWrite`: نوشتنِ کوچک پر می‌شود.
- `TestNewSessionShapesAnyStreamCarrier`: هر نشستی که از `newSession` ساخته شود شکل‌دهی دارد.

**core** (`core/core_test.go`، `injection_test.go`، `shape_test.go`، `datagram_test.go`):
- `TestHandshakeAndRoundTrip`، `TestWrongPSKFails`، `TestFirstMessageReplayRejected`، `TestGarbageFirstMessageRejected`.
- `TestReplayWindowDatagram`، `TestTamperedFrameFails`.
- `TestLengthPrefixIsMasked`: دو قاب هم‌طول پیشوند طول یکسانی ندارند.
- `TestManyHandshakesSameBucket`: 32 دست‌دهی در یک سطل زمانی پذیرفته می‌شوند و بیت بالای ephemeral ثابت نیست.
- `TestTCPInjectionRejectedQuietly`، `TestBitFlipInStreamRejected`.
- `TestPadDatagramTarget`: هیچ پرکردنی به MTU یا بیشتر نمی‌رسد و بار انبوه دست‌نخورده می‌ماند.
- `TestPayloadsBindingAndExporter`، `TestStaticFromSeedDeterministic`، `TestReplayWindowReorderAndJump`، `TestAppendDatagramMatchesSealDatagram`.

**reality** (`reality/*_test.go`):
- `TestBrowserProbeForwardedToCover`، `TestSignalledClientReachesTunnel`، `TestForgedSignalFails`.
- `TestSignalledHelloLooksLikeBrowser`: در عمل فقط بررسی می‌کند که هر دو ClientHello تجزیه‌پذیر باشند؛ مقایسهٔ ساختاری انجام **نمی‌شود** (م22).
- `TestPathA_EndToEnd`، `TestPathA_ProbeStillCovered`، `TestPathA_SingleTLSLayer`.

**cmd/hs2**:
- `TestCertReloaderHotSwap`: تعویض داغ کار می‌کند و فایل خراب گواهی خوب را جایگزین نمی‌کند.
- `TestCoverDeterministic`، `TestCoverSeedsDiffer`، `TestCoverSeedlessIsLegacy`، `TestCoverStructuralInvariants`، `TestCoverServedHeaders`، `TestCoverETagSaltedAndLegacyNone`، `TestCoverNeutralsVary`، `TestCoverBackendBothModes` (`cmd/hs2/cover_test.go`).

**نصاب:** `install/tests/cover_seed_test.sh` (seed مستقل از کلید، مهاجرت، یکتایی).

---

## ۱۱. «از قبل وجود دارد» (برای جلوگیری از دوباره‌کاری)

1. TLS 1.3 واقعی، با سرور `crypto/tls` استاندارد و گواهی واقعی Let's Encrypt، بدون هیچ دست‌کاریِ ClientHello در سمت سرور.
2. اثرانگشت کلاینت Chrome با utls v1.8.0 و `HelloChrome_133`. این شامل X25519MLKEM768، GREASE، GREASE ECH، ALPS، فشرده‌سازی گواهی با brotli و ترتیب پسوندهای به‌هم‌ریختهٔ Chrome است.
3. احراز دوطرفهٔ متصل به نشست (EKM). کلاینت پیش از اثبات سرور ساکت می‌ماند. ضد MITM است، حتی بدون وارسی زنجیرهٔ گواهی.
4. پرکنندهٔ تصادفی برای رکوردهای احراز و اثبات، به اندازهٔ درخواست و پاسخ HTTP.
5. حافظهٔ ضد replay با FIFO و هزینهٔ O(1)، و پنجرهٔ زمانی دقیقه‌ای ±2.
6. رد و لاگ‌کردن کلاینت v1.
7. «یک هویت منسجم» برای کاوشگر: پاسخ 400 دقیق Go برای HTTP خام (سنجاق‌شده به stdlib)، بستن با FIN یا RST مثل خود Go (تفاضلی آزموده شده)، و ارسال هر TLS کامل ولی احرازنشده به یک پشتیبان HTTP واقعی.
8. خواندن «یک رکورد»، نه تعداد بایت ثابت، تا کاوش کوتاه فوراً پاسخ بگیرد.
9. صفحهٔ پوششیِ مخصوص هر نصب: قطعی بر پایهٔ seed، بدون JS و درخواست بیرونی، با ETag نمک‌زده، `Last-Modified` نسبی، 404 برای هر مسیری جز `/`، و بدون سرآیند `Server`.
10. تعویض داغ گواهی با SIGHUP یا پایش mtime، هشدار انقضا، و تشخیص تمدید در `hs2 doctor`.
11. شکل‌دهی طول رکوردهای TLS در همهٔ حامل‌های جریانی با `shapedConn` و `LengthSampler`.
12. keepalive تصادفیِ smux (4 تا 8 ثانیه برای هر نشست).
13. فاصله‌گذاری دست‌دهی لینک‌ها در کل فرایند (حداکثر 8 هم‌زمان، 40 تا 160 میلی‌ثانیه فاصله).
14. تنظیم سوکت: BBR (یا cubic)، `TCP_NOTSENT_LOWAT`=32KiB، `TCP_USER_TIMEOUT`=20s، keepalive سه‌ثانیه‌ای در کلاینت، NODELAY، و خاموش‌بودن MPTCP.
15. `TCPConn()` زنجیره را کامل باز می‌کند تا `TCP_INFO` برای autopilot هیچ‌وقت خاموش نشود.
16. `bind_local_ip` بدون بازگشت بی‌صدا به IP پیش‌فرض.
17. در core: Noise IKpsk2، `firstMAC` پیش از Noise (ضد کاوش و ارزان)، حافظهٔ replay، تصادفی‌کردن بیت بالای ephemeral، پیشوند طول ماسک‌شده، پرکردن داخل AEAD، پنجرهٔ replay به سبک WireGuard، `Exporter`/`Binding`، `StaticFromSeed`، سطل‌های پرکردن datagram که هرگز به MTU نمی‌رسند، پرکنندهٔ دست‌دهی، و keepalive با jitter و پرکننده.
18. ابزار ارزیابی DPI به‌صورت اسکریپت پایتون (`obfs/dpi_eval*.py`).
19. (در زیرسیستم‌های دیگر) استتار ICMP پشت `HS2_ICMP_CAMO`، و کانال کنترل با آهنگ وابسته به بار.

---

## ۱۲. ایده‌هایی که امتحان یا بررسی و رد یا کنار گذاشته شده‌اند

| ایده | سرنوشت و دلیل | منبع |
|---|---|---|
| پیاده‌سازی کامل Reality (چندشاخه‌کردن `crypto/tls`، ساختن ClientHello بایت‌به‌بایت) | رد شد: شکننده نسبت به نسخه؛ «یک انحراف کوچک سرور را بی‌صدا می‌سوزاند» | `tlscarrier/doc.go:5-10` |
| بستهٔ `reality` (signal در session-id، گواهی محلی) | فقط آزمایشی ماند و نصاب آن را پیشنهاد نمی‌دهد | `BUILD.md:50, 155-156` |
| «مسیر 1» در Reality: رلهٔ کامل دست‌دهی سایت واقعی | نوشته شد ولی هرگز سیم‌کشی نشد (`relayProbe` فراخواننده ندارد) | `reality/relay.go` |
| غیر Go کردن پشتهٔ TLS در سرور (طراحی دوباره به سبک Reality، یا وابستگی به nginx) | عمداً انجام نشد: TLS باید در Go پایان یابد تا احراز متصل به EKM ممکن باشد، و این با طراحی تک‌باینری ایستا جور نیست | `CHANGELOG.md:150-155` |
| نگاه پیشاپیشِ 5 بایتی پیش از TLS، با `prefixConn` | حذف شد: کمتر از `crypto/tls` می‌خواند، پس FIN به RST تبدیل می‌شد؛ و `TCP_INFO` را خاموش کرده بود | `CHANGELOG.md:164-172, 218-227` |
| «تخلیه پیش از بستن» برای مسیر 400 | با حذف نگاه پیشاپیش کنار رفت | `CHANGELOG.md:226-227` |
| بستن بی‌صدای HTTP خام | با 400 دقیق Go جایگزین شد (بستن بی‌صدا یک اثرانگشت بود) | `CHANGELOG.md:116-131` |
| صفحهٔ خوش‌آمد nginx و سرآیند `Server: nginx` | حذف شد: با پشتهٔ TLSِ Go تناقض داشت و خودش امضای honeypot بود | `CHANGELOG.md:132-137`؛ `main.go:973-975` |
| یک صفحهٔ پوششیِ ثابت برای همه | با seed مخصوص هر نصب جایگزین شد: با یک sha256 همهٔ سرورها پیدا می‌شدند | `CHANGELOG.md:380-412` |
| ETag برابر `sha256(body)[:16]` | با نسخهٔ نمک‌زده جایگزین شد: با یک کاوش می‌شد قاعده را تأیید کرد | `CHANGELOG.md:417-422` |
| ثابت ماندن رنگ‌های خنثی در صفحهٔ پوششی | jitter گرفتند | `CHANGELOG.md:423-428` |
| سقف کاوشِ هر اتصال (probe shield) | به تعویق افتاد | `CHANGELOG.md:154-155`؛ `server.go:37-41, 45-49` |
| بالا بردن `firstReadTimeout` به 60 ثانیهٔ nginx | رد شد: goroutineها را زیر سیل کاوش بیشتر نگه می‌دارد و سود هماهنگیِ زمانی‌اش «نرم» است | `server.go:34-40` |
| لایهٔ AEAD دوم (Noise داخل TLS) | رد شد: هزینهٔ CPU بی‌هیچ امنیت اضافه، و اثرانگشت TLS-in-TLS | `BUILD.md:179-180`؛ `reality/carrier.go:10-15` |
| اعتبارسنجی زنجیرهٔ گواهی در کلاینت | انجام نشد: سمت ایران ممکن است ریشه‌های CA نداشته باشد و دامنه ممکن است fronted باشد؛ به‌جایش اتصال به EKM | `carrier.go:198-200` |
| احراز v1 (بدون اتصال به نشست) | با v2 جایگزین و رد می‌شود (ناسازگاری v3 با v2) | `auth.go:161-179`؛ `BUILD.md:158-162` |
| `firstMAC` فقط روی سطل زمانی | با نسخه‌ای که ephemeral را هم می‌پوشاند جایگزین شد: اتصال‌های مجددِ مشروع رد می‌شدند و پیشوند 16 بایتی تکرار می‌شد | `core/handshake.go:80-83` |
| پنجرهٔ replay با جابه‌جایی آرایهٔ 2048 تایی | با بیت‌نقشهٔ پیمانه‌ای جایگزین شد (memmove 2KiB به ازای هر بسته) | `core/replay.go:13-17` |
| پویش کامل نقشهٔ replay در tlscarrier | با FIFO و هزینهٔ O(1) جایگزین شد | `replaymem.go:9-12` |
| keepalive ثابت 5 ثانیه‌ای | با 4 تا 8 ثانیهٔ تصادفی برای هر نشست جایگزین شد (اثرانگشت زمانیِ بین نشست‌ها) | `mtcp_link.go:270-279`؛ `dpi_eval2.py:104-111` |
| کم کردن تعداد لینک‌های موازی | رد شد: «کاربران سنگین را خفه می‌کند». پذیرفته شده که همین بزرگ‌ترین نشانهٔ رفتاری است | `dpi_eval2.py:145-157`؛ `README.md:169-181` |
| پنجرهٔ پلکانیِ 2 تا 4 ثانیه برای برقراری لینک‌ها | ارزیابی گفت باید به حدود 1 ثانیه محدود شود (اکنون حدود 10 دست‌دهی در ثانیه) | `dpi_eval2.py:151-153` |
| شنوندهٔ MPTCP (پیش‌فرض Go 1.24 به بعد) | خاموش شد: `notsent_lowat` را نادیده می‌گیرد؛ p99 در آزمون 8 ثانیه بود در برابر 0.5 ثانیه | `engine/listen.go:21-27`؛ `CHANGELOG.md:827-861` |
| `NOTSENT_LOWAT` بزرگ‌تر از 64KiB یا خاموش | سنجیده شد و بدتر بود | `tune_linux.go:12-18` |
| تقلید یک پروتکل خاص («The Parrot is Dead») | در core رد شد؛ هدف «پاک‌کردن نظم‌ها»ست | `core/shape.go:14-16` |
| فریب (decoy) برای پیام اول نامعتبر در noise | «بعداً اضافه می‌شود» ولی هرگز پیاده نشد؛ اتصال فقط بسته می‌شود | `carrier_tcp.go:69-71` |

---

## ۱۳. محدودیت‌های شناخته‌شده و مشاهده‌ها

**محدودیت‌هایی که خودِ کد یا مستندات اعلام کرده‌اند:**
- در رژیم «فهرست سفید دامنه»، دامنه باید مجاز باشد. در برابر مسدودسازیِ SNI همان دامنه، باید دامنه را عوض کرد (`tlscarrier/doc.go:28-30`).
- پشتهٔ TLS سرور همان Go است. صفحهٔ پوششی فقط جلوی شمارش انبوه را می‌گیرد و «تغییر قیافه» نیست (`cmd/hs2/cover.go:28-32`؛ `README.md:791-800`).
- تعداد لینک‌های موازی به یک مقصد و طول عمرشان نشانه‌های رفتاری اصلی هستند (`dpi_eval2.py:142-157`؛ `README.md:169-181`).
- کلاینت گواهی را وارسی نمی‌کند، پس گواهیِ منقضی تونل را نمی‌خواباند ولی برای کاوشگر دیده می‌شود (`README.md:279-282`).

**مشاهده‌ها** (برداشت این نقشه، بدون پیشنهاد تغییر):

- **مشاهدهٔ م1، توزیع طول رکورد در مقایسه با HTTPS واقعی:** `LengthSampler` اندازهٔ «بسته» را مدل می‌کند (حدود 1400، یعنی MTU) ولی در واقع اندازهٔ **رکورد TLS** را تعیین می‌کند. در دانلود انبوه HTTPS معمولاً رکوردها حدود 16KiB هستند: پیش‌فرض nginx یعنی `ssl_buffer_size 16k`، و Go پس از 128KiB نخست (`recordSizeBoostThreshold`) هم 16384 می‌فرستد؛ این را در `crypto/tls/conn.go` در Go 1.27 دیدم. بنابراین جریانی پرحجم و طولانی که همهٔ رکوردهایش ≤1422 بایت است ممکن است نامعمول باشد. فرض `dpi_eval*.py` («HTTPS انبوه دو قله‌ای است با رکوردهای حدود 1400») این دو را یکی گرفته است. میزان استفادهٔ DPI واقعی از این نشانه: **نامطمئن**.
- **مشاهدهٔ م2، طیف گسسته:** طول رکوردهای روی سیم دقیقاً 9 مقدار ثابت دارند (62، 102، 172، 272، 422، 622، 922، 1222، 1422). این مقدارها در همهٔ نصب‌ها و هر دو جهت یکسان‌اند و نمودار فراوانیِ میله‌ای تیز می‌سازند. سرآیند رکورد TLS رمزنشده است، پس برای ناظر منفعل هم دیدنی است. آزمون فقط «دست‌کم 3 اندازهٔ متمایز» را تضمین می‌کند.
- **مشاهدهٔ م3، هزینهٔ CPU و syscall:** هر رکورد (به‌طور میانگین حدود 990 بایت) یک `tls.Conn.Write` جدا است، یعنی یک AEAD و یک syscall `send`. در توان بالا تعداد syscallها حدود 16 برابر حالت رکورد 16KiB است. سربار بایتی کم است (0.68٪ در آزمون). اثر واقعی روی گذردهی و CPU: **نامطمئن** (اندازه‌گیری نشد). این‌که هسته بخش‌های کوچک را در قطعه‌های TCP یکی می‌کند یا نه هم **نامطمئن** است: به cwnd و pacing بستگی دارد.
- **مشاهدهٔ م4، کاوشگر TLS 1.2 بدون EMS:** چون `MinVersion` برابر TLS1.2 است، دست‌دهی کامل می‌شود. ولی `exportEKM` خطا می‌دهد و سرور **بدون هیچ پاسخی** `raw.Close()` می‌کند (`server.go:103-108`). یک سرور HTTPS واقعیِ Go همان کلاینت را سرویس می‌داد. این ممیز برای کاوشگر فعال در کد قطعی است، ولی آزمونی آن را پوشش نمی‌دهد و ارزش عملی‌اش **نامطمئن** است (بیشتر کلاینت‌های امروزی EMS دارند).
- **مشاهدهٔ م5، ناهمخوانیِ زمانی:** کاوشگری که پس از TLS ساکت می‌ماند بعد از 30 ثانیه بسته می‌شود، ولی کاوشگری که سرآیند ناقص می‌فرستد به پشتیبان می‌رود که `ReadHeaderTimeout` آن 10 ثانیه است. در یک سرور Go واقعی هر دو تابع یک مهلت هستند. اهمیت عملی: **نامطمئن**.
- **مشاهدهٔ م6، ALPN:** سرور فقط `http/1.1` اعلام می‌کند (`server.go:78`)، در حالی که سرور پیش‌فرض `net/http` در Go و Caddy (هویتی که فاز C ادعا می‌کند) h2 را ترجیح می‌دهند. در TLS 1.3 پاسخ ALPN رمزشده است، پس فقط کاوشگر فعال آن را می‌بیند. با «سرور فقط HTTP/1.1» سازگار است.
- **مشاهدهٔ م7، نبودِ ازسرگیری نشست:** کلاینت utls کش نشست ندارد، پس همهٔ لینک‌ها دست‌دهی کامل (با Certificate) انجام می‌دهند. Chrome واقعی در اتصال‌های بعدی به همان میزبان معمولاً از PSK استفاده می‌کند. صدها دست‌دهی کامل به یک سرور ممکن است نامعمول باشد (**نامطمئن**).
- **مشاهدهٔ م8، پیر شدن اثرانگشت:** `HelloChrome_133` و utls v1.8.0 سنجاق شده‌اند. با تغییر نسخه‌های Chrome، این اثرانگشت کهنه می‌شود. نسخهٔ پایدارِ فعلی Chrome و تفاوت JA4 آن: **نامطمئن**. (reality با `HelloChrome_120` قدیمی‌تر است.)
- **مشاهدهٔ م9، اختلاف ساعت با پیام گمراه‌کننده:** اگر اختلاف ساعت از حدود 2 تا 3 دقیقه بیشتر شود، رکورد احراز به پشتیبان می‌رود و پشتیبان با `HTTP/1.x 400` جواب می‌دهد. کلاینت آن را `ErrOldServer` گزارش می‌کند که تقصیر را گردن `shared_key` یا نسخه می‌اندازد، نه ساعت. `hs2 doctor` رواداری را «حدود 1 دقیقه» می‌گوید که از واقعیت (±2 سطل) محافظه‌کارانه‌تر است.
- **مشاهدهٔ م10، نبود لاگ سمت سرور:** احراز ناموفق (کلید یا ساعت غلط) در لاگ سرور TLS هیچ ردی ندارد.
- **مشاهدهٔ م11، مهلت نوشتن smux:** نوشتن‌های smux روی `RawConn` مهلت ندارند. تشخیص گیر به `TCP_USER_TIMEOUT` (20s)، keepalive در smux (24s) و نگهبان wedge وابسته است.
- **مشاهدهٔ م12، keepalive نامتقارن:** سوکت‌های پذیرفته‌شده در سرور `SetKeepAlivePeriod(3s)` نمی‌گیرند و پیش‌فرض Go (حدود 15 ثانیه) را دارند. فقط `TCP_USER_TIMEOUT`=20s یکسان است. جزئیات پیش‌فرض Go: **نامطمئن**.
- **مشاهدهٔ م13، منابع کاوشگرهای ارسال‌شده:** `forward` مهلت بیکاریِ خودش را ندارد و پشتیبان داخلی `IdleTimeout` و `ReadTimeout` ندارد. پس اتصال keep-alive کاوشگر می‌تواند بی‌پایان دو goroutine و دو fd در tlscarrier و یکی در پشتیبان نگه دارد. خودِ کد هم گفته «probe shield» ساخته نشده است.
- **مشاهدهٔ م14، کلید replay:** کلید `replayMem` برابر XOR دو نیمهٔ nonce است (64 بیت). احتمال برخورد ناچیز است. در عمل replay به‌خاطر اتصال به EKM از قبل ناممکن است و این حافظه دفاع اضافه است.
- **مشاهدهٔ م15، حالت معکوس:** در حالت معکوس، سرور TLS با گواهی و پشتیبان پوششی روی IP ایران است و در برابر کاوشگرهای داخلی قرار دارد. همهٔ سازوکارهای بالا همان‌جا هم اجرا می‌شوند.
- **مشاهدهٔ م16، پشتیبان شکسته:** اگر `backend_addr` در دسترس نباشد، `forward` بی‌صدا بسته می‌شود (`server.go:166-169`). در این صورت کاوشگرِ پس از TLS هیچ پاسخی نمی‌گیرد و این با «هویت وب‌سایت» ناسازگار است.
- **مشاهدهٔ م17، کد مرده:** `obfs.Pacer`، `NextDelay`، `jitter`، `UDPHeaderPrefix`؛ فیلدهای `LengthSampler.mu` و `seeded`؛ `reality.relayProbe`/`pipe`، `authTag`/`verifyTag`، `clientHelloRecord`؛ `core.shapeSeed`؛ و در کد تولیدی `tlscarrier.SendFrame`/`SendFramePadded`/`ReadFrame` (پیوند L3 فقط روی `streamPkt` ساخته می‌شود: `engine/l3_link.go:126`).
- **مشاهدهٔ م18، مستندات و توضیح‌های ناهمخوان با کد:**
  - `BUILD.md:52` می‌گوید obfs «مال noise و reality است»، ولی در عمل فقط `LengthSampler` در مسیر جریانی استفاده می‌شود.
  - `obfs/design.md` فقط `placeholder` است.
  - `dpi_eval2.py` به `real_sizes.txt` نیاز دارد که در مخزن نیست، و هنوز از «صفحهٔ کاوش nginx» حرف می‌زند.
  - توضیح `server_probe_test.go:110` هنوز `prefixConn` را نام می‌برد.
  - `reality/server.go:29-34` جای برچسب را به شکل قدیمی توضیح می‌دهد.
  - `core/handshake.go:192` می‌گوید «سطل جاری یا قبلی» ولی کد ±1 را می‌پذیرد.
  - `core/shape.go:102` از «idNonce» حرف می‌زند.
  - `Session.id` «هنوز سیم‌کشی نشده» است.
- **مشاهدهٔ م19، پنجرهٔ زمانی core:** `tsWindow`=90s هیچ‌وقت محدودکننده نیست، چون فقط سطل‌های ±1 (هر سطل 30 ثانیه) پذیرفته می‌شوند و `withinWindow` همیشه درست برمی‌گرداند. پنجرهٔ مؤثر حدود 30 تا 60 ثانیه است. `replayMemory` در core وقتی از 8192 بگذرد در هر `add` یک پویش O(n) می‌کند و فقط موارد منقضی را حذف می‌کند، پس زیر سیلِ MACهای معتبر (که psk لازم دارد) سقفِ سخت ندارد. نسخهٔ tlscarrier این مشکل را با FIFO حل کرده، ولی core نه.
- **مشاهدهٔ م20، اثرانگشت دست‌دهی noise روی TCP:** بایت‌های 2 و 3 روی سیم (`inner`) طول پیام Noise را به‌صورت رمزنشده و **ثابت** نشان می‌دهند: m1 برابر 112 و m2 برابر 48، طبق محاسبهٔ IK (96+16 و 32+16). پس توضیح «دیگر همیشه 112 و بعد 48 نیست» (`carrier_tcp.go:96-99`) فقط دربارهٔ طول کل درست است. همچنین `noiseListener.Accept` دست‌دهی را پشت سر هم انجام می‌دهد، پس یک کلاینت کند تا 10 ثانیه پذیرش بقیه را معطل می‌کند. حامل noise در نصاب نیست.
- **مشاهدهٔ م21، reality:**
  - حافظهٔ replay برای session-id ندارد. ClientHelloِ مجازِ ضبط‌شده اگر بازپخش شود دست‌دهی با **گواهی محلی** می‌گیرد، نه سایت پوششی. این ممیز شناخته‌شدهٔ این طراحی است.
  - `realityCarrier.SendFrame` اگر `SetWriteDeadline` خطا بدهد، بی‌صدا `nil` برمی‌گرداند و چیزی نمی‌نویسد (`engine/carrier_reality.go:37`).
  - Pong بدون پرکننده است.
- **مشاهدهٔ م22، آزمون reality:** `TestSignalledHelloLooksLikeBrowser` با وجود نامش هیچ مقایسهٔ ساختاری ندارد.
- **مشاهدهٔ م23، اندازه‌گذاری پویای رکورد در Go:** این قابلیت (رکورد اول ≤1186 بایت، بعد رشد حسابی) در عمل اثری ندارد، چون اولین رکوردهای داده (احراز ≤754 و اثبات ≤498) کوچک‌اند و از رکورد دوم به بعد سقف ≥2372 است. پس نمونه‌های 1400 شکسته نمی‌شوند. این را در `crypto/tls/conn.go:895-940` بررسی کردم.

---

## ۱۴. ارجاع به زیرسیستم‌های دیگر

**این لایه چه چیزی را صدا می‌زند:**
- `utls` v1.8.0 (`HelloChrome_133`/`_120`)، `crypto/tls`، `golang.org/x/crypto/blake2s`، `chacha20`، `chacha20poly1305`، `flynn/noise` v1.1.0، `golang.org/x/sys/unix` (setsockopt).
- `obfs.LengthSampler` از `engine/shape.go` صدا زده می‌شود.

**چه کسی این لایه را صدا می‌زند:**
- `tlscarrier.DialFrom`: `engine/mtcp_link.go:287` (لبهٔ مستقیم) و `cmd/hs2/main.go:531-536` (خروجیِ معکوس، از راه `engine/exit_pool.go`).
- `tlscarrier.Server.Handle`: `engine/stream_kharej.go:126` (خروجیِ مستقیم) و `engine/stream_reverse.go:64` (لبهٔ معکوس).
- `Carrier.RawConn`: `newSession` (`engine/stream.go:172`)، `newEdgeLink` (`mtcp_link.go:301`)، `serveReverseLink` (`stream_reverse.go:150`).
- `Carrier.TCPConn`: `engine/stats.go:251-256` و `mtcp_link.go:52-57`، برای خواندن `TCP_INFO` که سلامت لینک و autopilot از آن استفاده می‌کنند (زیرسیستم مدیر لینک و سلامت).
- `tlscarrier.AppendFrame`: `engine/l3_link.go:210, 235` (کانال جانبی hs0 در l3mtcp؛ زیرسیستم L3).
- `tlscarrier.NotSentLowat` و `CongestionControl`: `cmd/hs2/main.go:267, 272, 358`، و `tune/tune.go` (زیرسیستم تنظیم هسته).
- `core`: `udpcarrier/listen.go:184-188`، `udpcarrier/dial.go:214`، `udpcarrier/carrier.go:778` (`PadDatagramTarget`)، `engine/carrier_tcp.go`، `engine/carrier_noise.go`، `engine/engine.go:143-197` (موتور قدیمی)، `engine/dgpool.go` (`core.Type*`).
- گواهی و پوشش: `cmd/hs2/cert.go`، `cmd/hs2/cover.go`، `cmd/hs2/main.go:781-789, 945-993`؛ و نصاب (`install/install.sh:1360-1384, 1851-1855`) و `hs2 doctor` (`cmd/hs2/doctor_cert.go`، `doctor.go:74-77`).

**هم‌پوشانی با زیرسیستم‌های دیگر:**
- بالای `shapedConn`: `meteredConn` (`engine/health.go:170`) و `watchConn` و نگهبان wedge (`engine/stream.go:77-123`، `engine/wedge.go`).
- آهنگ کانال کنترل (`engine/control.go:38-48`).
- مدیر لینک و autopilot (`engine/linkmanager.go`) و دروازهٔ شماره‌گیری (`engine/dialgate.go`).
