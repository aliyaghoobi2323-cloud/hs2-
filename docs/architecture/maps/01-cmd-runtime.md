# راه‌اندازی و سیم‌کشی در cmd/hs2

> دامنه: همهٔ فایل‌های `hs2-src/cmd/hs2` (کد و آزمون). مسیرها نسبت به `hs2-src/` هستند مگر خلافش گفته شود.
> وضعیت مخزن هنگام بررسی: شاخهٔ main، ثبت `0812bc9`، درخت تمیز؛ `go test ./cmd/hs2/` با ۷۷ آزمون سبز است.
> فایل‌های خوانده‌شده به‌طور کامل: `main.go`، `config.go`، `check.go`، `cert.go`، `cleanup.go`، `pprof.go`، `hostcpu.go`، `ports.go`، `status.go`، `doctor.go`، `doctor_cert.go`، `cover.go` و همهٔ `*_test.go` همین پوشه (به‌جز بدنهٔ جزئی `doctor_cert_test.go` که فقط فهرست آزمون‌ها و توضیحاتش مرور شد).

---

## ۰. خلاصهٔ اجرایی (برای مسیر اصلی l3mtcp)

- `cmd/hs2` خودش هیچ منطق داده‌ای ندارد؛ کارش «سیم‌کشی» است: خواندن JSON، تعیین نقش (لبه/خروجی) و جهت (direct/reverse)، باز کردن TUN، ساخت دیالر یا شنوندهٔ TLS، تعیین پاکت پیوندها (`min/max/per_link`)، شروع نوشتن وضعیت زنده، تنظیم هسته و حافظه، و تحویل همه‌چیز به `engine.RunIran` / `engine.RunKharej`.
- برای `l3mtcp` (و نام مستعار `l3`): `runStream(ctx, fc, withTUN=true, links=0)` در `cmd/hs2/main.go:410-411`. TUN با `tun.Open` و MTU پیش‌فرض **1380** باز می‌شود (`main.go:458-466`)، **بدون** offload (برخلاف dgtun). TUN در این حالت فقط «کانال جانبی» است؛ ترافیک کاربر روی جریان‌های smux پورت‌های کاربر می‌رود (README.md:659-666 در ریشهٔ مخزن).
- پاکت پیوندها: `min_links` پیش‌فرض 2، `per_link` پیش‌فرض 8، سقف از `linkCeiling`: مثبت = ثابت، `0` = خودکار از سخت‌افزار (حداکثر 300)، غایب = 32 تاریخی (`main.go:644-724`).
- راه‌اندازی گرم: لبه اندازهٔ هدف پیش از ری‌استارت را از `/run/hs2/<...>.warm` می‌خواند (حداکثر ۱۵ دقیقه کهنگی، فقط بالاتر از ۸)، `main.go:280-293` و `status.go:192-246`.
- حلقهٔ وضعیت هر ۲ ثانیه علاوه بر نوشتن فایل وضعیت، دو سیگنال به مسیر داده می‌دهد: فشار حافظهٔ TCP هسته (`engine.SetTCPMemPressure`) و اشباع CPU کل سرور (`udpcarrier.SetHostSaturated`) — `status.go:330` و `status.go:854`.

---

## ۱. نقش و جایگاه در کل سیستم

- `hs2` یک باینری ایستا با زیرفرمان‌های `version`، `keygen`، `run`، `check`، `doctor`، `status`، `tune`، `recommend-links`، `config`، `ports`، `cleanup` است (`main.go:214-250`).
- فقط `run` دیمن تونل است؛ بقیه ابزار عملیاتی‌اند که نصب‌کننده (`install.sh` در ریشهٔ مخزن) یا اپراتور صدا می‌زند.
- سرویس systemd که نصب‌کننده می‌سازد: `ExecStart=$BIN run -c $CFG`، `ExecReload=/bin/kill -HUP $MAINPID`، `Restart=always`، `RestartSec=3`، `TimeoutStopSec=8`، `KillMode=mixed`، `LimitNOFILE=1048576` (install.sh:836-845).
- نقش‌ها ثابت‌اند و از `mode` می‌آیند: `"dial"` = لبه/ایران (پورت‌های کاربر)، `"listen"` = خروجی/خارج (پنل). کلید `reverse` فقط «چه کسی TLS را شماره می‌گیرد» را برمی‌گرداند؛ نقش‌ها عوض نمی‌شوند (`main.go:53-57`، `main.go:427-430`).

---

## ۲. اجزای اصلی

### ۲.۱ main.go

| شناسه | محل | کار |
|---|---|---|
| `fileConfig` | `main.go:36-105` | ساختار کامل پیکربندی JSON (جدول کلیدها در بخش ۹) |
| `fileConfig.UnmarshalJSON` | `main.go:110-121` | رمزگشایی عادی + ثبت اینکه `max_links` اصلاً بوده یا نه (`maxLinksSet`)؛ `null` = غایب |
| `baseVersion` | `main.go:127` | خط قابلیت؛ پیشوند `"hs2 v3 ("` قرارداد نصب‌کننده است و نباید عوض شود |
| `buildTag`، `buildStamp`، `versionLine` | `main.go:133-212` | مهر ساخت از متادیتای VCS؛ برچسب اختیاری `-X main.buildTag`؛ حذف `[` `]` و نویسه‌های کنترلی |
| `main` | `main.go:214-250` | توزیع زیرفرمان؛ `run` اول `startPprof()` و بعد `runCmd` |
| `configPath` (متغیر سراسری) | `main.go:254` | مسیر پیکربندی دیمن برای مشتق کردن مسیر فایل وضعیت و گرم |
| `applyTuning` | `main.go:258-275` | بازنویسی آزمایشگاهی چند عدد مسیر داده با متغیر محیطی (بخش ۹) |
| `warmLinks` | `main.go:280-293` | خواندن فایل گرم، گیره در `[min,max]`، فقط اگر از `engine.WarmSize` بیشتر باشد |
| `clampInt` | `main.go:295-303` | گیره |
| `setMemoryLimit` | `main.go:309-320` | حد نرم حافظهٔ Go = نصف RAM (مگر `GOMEMLIMIT` تنظیم باشد) |
| `runCmd` | `main.go:322-425` | بدنهٔ دیمن (بخش ۳.۲) |
| `dialing` | `main.go:430` | `(Mode=="dial") != Reverse` |
| `runReality` | `main.go:432-444` | حامل آزمایشی reality (گواهی بدون بارگذاری مجدد داغ) |
| `runStream` | `main.go:453-548` | **حامل‌های mtcp/l3mtcp/tls** (بخش ۳.۳) |
| `buildTunePlan`، `tuneSkipReason`، `tuneCmd` | `main.go:552-592` | طرح تنظیم هسته |
| `recommendLinksCmd` | `main.go:604-624` | چاپ سقف خودکار پیوند برای نصب‌کننده |
| `ramStr` | `main.go:627-635` | قالب RAM |
| `linkEnvelope` | `main.go:644-657` | `(min, max, per)` با پیش‌فرض‌ها |
| `detectHW` (متغیر) | `main.go:661` | `= tune.Detect`؛ در آزمون‌ها سنجاق می‌شود |
| ثابت‌های `ceilAuto/ceilFixed/ceilDefault/ceilICMP` | `main.go:664-669` | چهار حالت سقف |
| `icmpMaxLinks = 8` | `main.go:678` | سقف تونل dgtun روی icmp |
| `isICMPTun` | `main.go:681-683` | `dgtun` + `encap` برابر icmp (حساس نبودن به بزرگی حروف و فاصله) |
| `legacyMaxLinks = 32` | `main.go:686` | سقف تاریخی وقتی `max_links` غایب است |
| `linkCeiling` | `main.go:711-724` | قاعدهٔ سقف (بخش ۶.۵) |
| `ceilingLogLine` | `main.go:729-753` | یک خط لاگ شروع دربارهٔ سقف |
| `hasLinkPool` | `main.go:758-764` | mtcp/l3mtcp/l3/dgtun |
| `drainIdle` | `main.go:768-777` | نگاشت `drain_idle_sec` به تنظیم موتور |
| `streamBackend` | `main.go:781-789` | پشتیبان پروب: `backend_addr` یا وب‌سایت داخلی |
| `runUDP` | `main.go:795-821` | udp و auto |
| `runDgTun` | `main.go:829-903` | TUN دیتاگرامی (بخش ۳.۸) |
| `engineDialsTransport` | `main.go:907` | همان فرمول `dialing` (تکراری) |
| `encapName`، `ipOfCIDR` | `main.go:910-923` | کمکی |
| `runNoise` | `main.go:925-935` | حامل noise (وقتی `carrier` خالی است) |
| `startBuiltinBackend` | `main.go:945-993` | سرور HTTP پوششی روی `127.0.0.1:0` |
| `splitComma`، `unhex`، `must` | `main.go:995-1011` | کمکی؛ `must` پیش از `log.Fatal` قواعد echo guard را آزاد می‌کند |

### ۲.۲ status.go (فایل وضعیت زنده، گرم، متر CPU، فشار حافظهٔ TCP)

| شناسه | محل | کار |
|---|---|---|
| `statusInterval = 2s` | `status.go:29` | دورهٔ نوشتن وضعیت |
| `statusRunDir = "/run/hs2"` | `status.go:33` | پوشهٔ tmpfs |
| `liveStatus` | `status.go:37-177` | JSON تخت وضعیت (بخش ۷) |
| `statusPath` | `status.go:182-190` | `/run/hs2/` + مسیر مطلق با `/`→`-` و فاصله→`_` + `.status.json` |
| `warmPath`، `warmMaxAge=15m`، `readWarm` | `status.go:195-220` | فایل گرم |
| `warmWriter`، `warmAfter=1m`، `note` | `status.go:224-246` | نوشتن فایل گرم |
| `startStatusWriter` | `status.go:250-349` | **goroutine** حلقهٔ وضعیت (بخش ۵.۱) |
| `cpuMeter`، `clkTck=100`، `sample` | `status.go:355-404` | مصرف CPU خود فرایند |
| `writeStatusFile` | `status.go:408-418` | نوشتن اتمی (tmp + rename) |
| `statusCmd`، `printStatus` | `status.go:422-536` | داشبورد `hs2 status [--watch]` |
| `patternLine`، `effectiveCeiling`، `ceilingWhy`، `ceilingLine`، `unreported`، `driftLine`، `hwWhy`، `whyLine`، `trafficLine` | `status.go:549-774` | متن‌سازی |
| `role`، `direction`، `carrierName`، `transportLabel`، `endpointLabel` | `status.go:780-829` | برچسب‌ها؛ `carrierName` خالی را `noise` می‌کند |
| `tcpMemWatch.check` | `status.go:834-855` | پایش `tcp_mem` و تغذیهٔ `engine.SetTCPMemPressure` |
| `cpuLine`، `poolLine`، `sendingLine`، `offloadLine` | `status.go:860-911` | متن‌سازی |

### ۲.۳ hostcpu.go (متر کل سرور)

`hostMeter` (`hostcpu.go:29-40`) با `sample` (`:187-212`) و `judge` (`:214-242`)؛ خواندن `/proc/stat` (`:78-109`)، `/proc/pressure/cpu` (`:128-155`)، `/proc/net/snmp` OutDiscards (`:159-183`)؛ آستانه‌ها در `:58-65`.

### ۲.۴ cert.go (گواهی داغ)

`certReloader` (`cert.go:19-25`)، `certRegistry` سراسری (`:28-31`)، `newCertReloader` (`:35-44`، اولین بارگذاری بد = خطای کشنده)، `load` (`:47-65`)، `getCertificate` (`:68-70`)، `reload` (`:73-84`، بارگذاری بد گواهی قبلی را نگه می‌دارد)، `changedOnDisk` (`:87-95`، mtime)، `daysLeft` (`:98-104`)، `reloadAllCerts` (`:107-114`)، `watchCerts` (`:118-142`، **goroutine** هر دقیقه)، `firstCertExpiryDays` (`:146-156`).

### ۲.۵ check.go، config.go، ports.go

- `checkCmd`/`checkConfig` (`check.go:31-368`)، `checkExitTable` (`:375-410`)، `checkPoolBounds` (`:460-473`)، `knownCarriers` (`:71-75`)، `knownEncaps` (`:90-92`)، `ipxHandledProtos` (`:79-84`)، `haveEchoGuardTool` (`:441-448`).
- `configCmd` و لیست سفید `configKeys` (`config.go:39-59`، `:61-120`)؛ `readConfigMap` با `UseNumber` (`:124-139`)؛ `writeFileAtomic` (`:222-228`).
- `portsCmd`/`editPorts` (`ports.go:259-443`)، نمای جدول مسیریابی (`portLines` `:133-248`)، `checkPorts` برای doctor (`:473-559`)، `listeningSockets` از `/proc/net/{tcp,tcp6,udp,udp6}` (`:573-609`).

### ۲.۶ doctor.go و doctor_cert.go

`doctorCmd` (`doctor.go:29-85`) با بررسی‌های: config، running، endpoint، certificate، cert renewal، tun، kernel tuning، link pool ceiling، link count visibility، user ports، kernel TCP memory، conntrack، server cpu، tunnels together، clock. جزئیات تمدید گواهی certbot در `doctor_cert.go:15-446`.

### ۲.۷ cover.go (وب‌سایت پوششی)

`buildCover(seed, now)` (`cover.go:250-450`) صفحهٔ HTML تک‌فایلی و بدون جاوااسکریپت می‌سازد؛ مولد تصادفی قطعی بر پایهٔ SHA-256 (`cover.go:47-126`)؛ صفحهٔ ثابت قدیمی `coverFixedPage` برای seed خالی (`cover.go:540-662`). این فایل فقط در مسیر پروب (احراز نشده) معنا دارد.

### ۲.۸ cleanup.go، pprof.go

- `cleanupCmd` (`cleanup.go:20-32`): پاک‌سازی قواعد echo guard حامل icmp که صاحبشان مرده؛ `olderHs2Running` با `/proc/*/exe` (`:37-57`).
- `startPprof` (`pprof.go:20-43`): فقط آدرس loopback در `HS2_PPROF`.

### ۲.۹ goroutineهایی که cmd راه می‌اندازد (در `run`)

| goroutine | محل | پایان |
|---|---|---|
| نگهبان خاموشی: پس از لغو ctx، ۳ ثانیه خواب، `ReleaseAllEchoGuards`، `os.Exit(0)` | `main.go:380-385` | خروج فرایند |
| گیرندهٔ SIGHUP → `reloadAllCerts("SIGHUP")` | `main.go:391-402` | ctx |
| `watchCerts` (هر ۱ دقیقه) | `main.go:403`، `cert.go:118` | ctx |
| حلقهٔ وضعیت (هر ۲ ثانیه) | `status.go:335-348` | ctx (و حذف فایل وضعیت) |
| سرور HTTP پوششی | `main.go:991` | هرگز (تا خروج) |
| سرور pprof (اختیاری) | `pprof.go:42` | هرگز |

---

## ۳. جریان داده و کنترل، گام‌به‌گام

### ۳.۱ ورود

1. `log.SetFlags(log.Ltime | log.Lmicroseconds)` — همهٔ لاگ‌ها با زمان میکروثانیه (`main.go:215`).
2. `run` → `startPprof()` → `runCmd(os.Args[2:])` (`main.go:227-229`).

### ۳.۲ runCmd (مشترک همهٔ حامل‌ها) — `main.go:322-425`

1. `defer encap.ReleaseAllEchoGuards()` (`:325`).
2. `applyTuning()` — خواندن `HS2_TUNE_*` (`:326`).
3. `setMemoryLimit()` — حد نرم Go = نصف RAM تشخیص‌داده‌شده (با محدودیت cgroup) (`:327`).
4. خواندن فایل و `json.Unmarshal` با `must` (JSON بد = خروج کشنده) (`:328-335`).
5. اگر `bind_local_ip` نامعتبر است → `log.Fatalf` (`:339-341`)؛ اگر تنظیم است، لاگ «egress» (`:342-344`).
6. `buildTunePlan(fc)`؛ اگر root و `HS2_NO_TUNE` خالی → `plan.Apply` (sysctlها)، وگرنه فقط لاگ خلاصه (`:351-356`).
7. اگر `HS2_TUNE_CC` تنظیم نشده و طرح congestion دارد → `tlscarrier.CongestionControl = plan.Congestion` (کنترل ازدحام سوکت‌های خود تونل) (`:357-359`).
8. اگر حامل پول دارد → لاگ سقف (`ceilingLogLine`) (`:360-362`).
9. برای udp/auto/dgtun با `mtu` صفر → 1280 (`:367-369`).
10. `engine.New(...)` — فقط یک ساختار بی‌اثر می‌سازد؛ برای حامل‌های stream استفاده نمی‌شود (`main.go:371-373`، `engine/engine.go:48-56`).
11. `signal.NotifyContext` برای SIGINT/SIGTERM + نگهبان خاموشی ۳ ثانیه‌ای (`:375-385`).
12. SIGHUP و `watchCerts` (`:391-403`).
13. `switch fc.Carrier` (`:405-424`): `reality`، `mtcp`→`runStream(false,0)`، `l3mtcp`/`l3`→`runStream(true,0)`، `tls`→`runStream(true,1)`، `noise`/`""`، `udp`، `auto`، `dgtun`؛ بقیه `log.Fatalf("unknown carrier %q")`.

> نکته: `run` خودش `checkConfig` را اجرا نمی‌کند؛ `mode` نامعتبر، `shared_key` غیرهگز و … در زمان اجرا رد نمی‌شوند (بخش ۱۳).

### ۳.۳ runStream — مسیر l3mtcp از main تا موتور (`main.go:453-548`)

مقدمات مشترک:
- `key := unhex(fc.SharedKey)` (خطای هگز نادیده گرفته می‌شود) (`:454`).
- `withTUN=true` → `tun.Open(fc.Iface, fc.LocalCIDR, fc.PeerIP, mtu)` با `mtu = fc.MTU` یا 1380؛ `defer d.Close()`؛ لاگ `tun %s up: %s peer %s (side channel)` (`:457-467`). `tun.Open` همان `OpenWith(..., Options{})` یعنی **بدون offload** است (`tun/tun_linux.go:67-69`)، و پیش از ساخت، رابط هم‌نام کهنه را با `ip link del` پاک می‌کند (`tun/tun_linux.go:100-102`).
- `edge := fc.Mode == "dial"` (`:468`).

چهار شکل:

**الف) لبهٔ direct (ایران، پیش‌فرض)** — `:470-502`
1. `min, max, per := linkEnvelope(fc)`.
2. `engine.IranConfig{Min, Max, PerLink, DrainIdle: drainIdle(fc), ListenIP: user_listen_ip, Ports: forward_ports, UDP, Log, OnStart: startStatusWriter}`.
3. `cfg.WarmLinks = warmLinks(...)` (فقط وقتی `links==0` یعنی نه برای tls).
4. `cfg.TUN = dev`.
5. `cfg.Dialer = engine.NewMTCPDialer(fc.Addr, fc.SNI, key, fc.BindLocalIP)` (`engine/mtcp_link.go:315-317`) — هر پیوند = `tlscarrier.DialFrom` (اتصال TCP با مهلت ۸ ثانیه، `tlscarrier/carrier.go:161-163`) + جلسهٔ smux کلاینت.
6. `must(engine.RunIran(ctx, cfg))`.

**ب) لبهٔ reverse (ایران گوش می‌دهد)** — `:489-498`
1. همان پاکت و گرم.
2. `newCertReloader(cert_file, key_file)` (گواهی لازم است چون لبه سرور TLS است) + `engine.ListenReuse(fc.Addr)` (SO_REUSEADDR، بدون MPTCP؛ `engine/listen.go:12-44`).
3. `cfg.RevServer = &tlscarrier.Server{SharedKey, GetCertificate: cr.getCertificate, BackendAddr: streamBackend(fc), Logf}`؛ `cfg.RevListener = ln`.
4. لاگ `stream edge (reverse): listening for kharej links on %s`.
5. `RunIran`؛ در موتور، پول با `NewLinkManager(nil, cfg.Min, cfg.Max, ...)` و `accept=true` ساخته می‌شود و اتوپایلوت همچنان تعداد را تصمیم می‌گیرد و از کانال pool-control به خروجی می‌گوید (`engine/stream_iran.go:63-70`، `:124-126`).

**ج) خروجی direct (خارج گوش می‌دهد، پیش‌فرض)** — `:506-547`
1. `_, exitMax, _ := linkEnvelope(fc)`؛ برای tls برابر 1.
2. `routes := engine.ParsePortMap(fc.PortMap)` (خطا = کشنده).
3. `engine.KharejConfig{Panel: expose, Routes, Log, MaxLinks: exitMax, OnStart: startStatusWriter, TUN: dev}`.
4. `newCertReloader` + `ListenReuse(fc.Addr)` + `tlscarrier.Server{..., BackendAddr: streamBackend(fc)}`.
5. `must(engine.RunKharej(ctx, cfg))`. در direct، `MaxLinks` خروجی فقط گزارشی است و اعمال نمی‌شود؛ لبه به‌تنهایی تعداد را تعیین می‌کند (`engine/stream_kharej.go:49-53`).

**د) خروجی reverse (خارج شماره می‌گیرد)** — `:520-539`
1. `min, max, _ := linkEnvelope(fc)` (برای tls برابر 1,1).
2. `cfg.RevMin, cfg.RevMax = min, max`؛ `cfg.RevLinks = engine.WarmSize(min, max)` = گیرهٔ عدد ۸ در `[min,max]` (`engine/linkmanager.go:742-751`، `engine/health.go:85`). **خروجی فایل گرم را نمی‌خواند.**
3. `cfg.RevDial = tlscarrier.DialFrom(addr, sni, key, bind_local_ip)` (مهلت اتصال ۸ ثانیه).
4. `cfg.RevDialScout = tlscarrier.DialFromTimeout(..., 2*time.Second)` — برای تنها اسلاتی که هنگام نبود هیچ پیوند پیوسته تلاش می‌کند (`engine/stream_kharej.go:35-40`).
5. لاگ `stream exit (reverse): dynamic link pool %d–%d to edge %s (edge drives the count)`؛ `RunKharej` → `runKharejReverse` (`engine/stream_reverse.go:109`).

پس از ورود به موتور (خلاصه، برای ارجاع):
- لبه: برای هر پیوند تازه `OnLink` چهار کار می‌کند: کانال control (سلامت)، کانال stats (فشار سمت دانلود خروجی)، تبادل `peerInfo` (سقف، پورت‌های کاربر، پرچم UDP، قابلیت‌های `capPortTags|capL3Quiet`) و در reverse، کانال pool-control؛ و در l3mtcp، `openL3` یک جریان smux خام با نوع `kindL3` روی همان پیوند باز می‌کند (`engine/stream_iran.go:81-128`، `:419-438`).
- `l3Set.pumpTun` بسته‌ها را از TUN می‌خواند و با درهم‌سازی rendezvous روی ۵-تایی جریان، به صف همان پیوند می‌دهد؛ صف پر یا نبود پیوند = دور ریختن (`engine/l3_link.go:308-358`). ثابت‌های این مسیر: صف ۲۵۶ بسته، دسته ۱۶ KiB، حداکثر ماندن در صف **۶۰ ms**، keepalive ۲ ثانیه (یا ۱۰ ثانیه ±۲۰٪ در حالت quiet)، مهلت مرگ جریان ۳۰ ثانیه، و جابه‌جایی جریان‌ها اگر کل جلسه ۱۲ ثانیه ساکت باشد (`engine/l3_link.go:49-88`).
- پورت‌های کاربر: برای هر پورت `ListenReuse` + حلقهٔ accept با backoff؛ با `udp:true` یک `ListenPacket` هم؛ لاگ `user port %s open (%s), carried over the link pool` (`engine/stream_iran.go:138-172`). خطای bind پورت کاربر از `RunIran` برمی‌گردد → `must` → خروج کشنده.

### ۳.۴ زنجیرهٔ تصمیم پاکت پیوندها

```
fileConfig.min_links ─┐
fileConfig.per_link ──┼─ linkEnvelope ─► (min, max, per) ─► IranConfig{Min,Max,PerLink} یا KharejConfig{RevMin,RevMax}/MaxLinks
fileConfig.max_links ─┴─ linkCeiling(detectHW) ─► max (اگر max<min → max=min)
/run/hs2/*.warm ──────── warmLinks ─► IranConfig.WarmLinks (فقط لبه، فقط >WarmSize)
```

### ۳.۵ حلقهٔ وضعیت و بازخورد به مسیر داده

`OnStart(StatsFn)` از درون موتور یک‌بار صدا زده می‌شود (`engine/stream_iran.go:130-132`) و `startStatusWriter` را راه می‌اندازد. هر ۲ ثانیه (`status.go:279-334`):
1. `tcpMem.check()` → اگر لازم لاگ + `engine.SetTCPMemPressure(above)`.
2. `stats()` از موتور؛ پر کردن فیلدهای پول، سقف مؤثر، جدول پورت‌ها، ترافیک.
3. اگر پول لبه است (`s.Max>0` و فاز نه `following`/`listening`) → `warm.note(s.Target)` و جزئیات serving/retiring/… .
4. اگر پول دیتاگرامی است → فیلدهای از دست رفت/FEC/… .
5. `cpu.sample()` و `host.sample()`؛ سپس `udpcarrier.SetHostSaturated(hs.Saturated)` (فقط روی حامل‌های دیتاگرامی اثر دارد).
6. `firstCertExpiryDays()`، `Updated`، نوشتن اتمی.
با لغو ctx فایل وضعیت پاک می‌شود (`status.go:341-343`)؛ فایل گرم می‌ماند.

### ۳.۶ چرخهٔ گواهی

`newCertReloader` → ثبت در `certRegistry` → `tls.Config.GetCertificate` هر دست‌دهی آخرین گواهی را می‌گیرد → SIGHUP یا تغییر mtime (پایش دقیقه‌ای) → `reload`؛ بارگذاری ناموفق گواهی فعلی را نگه می‌دارد و لاگ می‌کند. هشدار انقضا در ≤۷ روز (یک‌بار، تا تغییر فایل) (`cert.go:118-142`). `runReality` گواهی را مستقیم با `tls.LoadX509KeyPair` می‌خواند و در رجیستری نیست (`main.go:439`).

### ۳.۷ خاموشی و خطا

- SIGINT/SIGTERM → لغو ctx → موتور جمع می‌شود؛ اگر تا ۳ ثانیه خارج نشد، `os.Exit(0)` (`main.go:380-385`). systemd پس از ۸ ثانیه KILL می‌کند.
- هر `must(err)` = آزادسازی echo guard و `log.Fatal` (کد خروج ۱) → systemd پس از ۳ ثانیه دوباره راه می‌اندازد.

### ۳.۸ مسیرهای دیگر (برای مقایسه)

- `runDgTun` (`main.go:829-903`): `tun.OpenWith(..., Options{Offload: HS2_TUN_OFFLOAD != "0"})`، MTU پیش‌فرض 1280، `engine.EncapConfig{Kind: encap, BindIP, Proto}`، پیش‌برندهٔ پورت در فضای کاربر (`engine.StartDgPorts`) **پیش از** پول، `DgConfig{Dev, Min, Max, PerLink, Reverse, ...}`، گرم فقط برای `Mode=="dial"`، دیالر یا شنونده بر اساس `engineDialsTransport`، و `RunDgEdge`/`RunDgExit`. `OnStart` آمار offload و جدول مسیر طرف مقابل را به وضعیت اضافه می‌کند.
- `runUDP` (`:795-821`): Noise + FEC روی UDP؛ `auto` اول UDP را می‌آزماید و در غیر این صورت به TCP noise برمی‌گردد. کلیدها از `shared_key` مشتق می‌شوند.
- `runNoise` (`:925-935`): `local_priv/local_pub/remote_static/psk`.
- `runReality` (`:432-444`): آزمایشی؛ در `baseVersion` نام برده نمی‌شود و BUILD.md آن را «not used by default» می‌خواند.

---

## ۴. جدول ثابت‌ها، آستانه‌ها، اندازه‌ها و زمان‌سنج‌ها

| نام | مقدار | محل | معنی |
|---|---|---|---|
| مهلت خاموشی | 3 s | `main.go:382` | پس از سیگنال، خروج اجباری |
| MTU پیش‌فرض TUN در stream | 1380 | `main.go:459-461` | l3mtcp/tls وقتی `mtu` صفر است |
| MTU پیش‌فرض udp/auto/dgtun | 1280 | `main.go:367-369`، `:798-800`، `:832-834` | جا شدن بستهٔ مهروموم‌شده با FEC در مسیر 1500 |
| MTU پیش‌فرض `engine.New` | 1380 | `engine/engine.go:49-51` | برای noise/udp/reality |
| `min_links` پیش‌فرض | 2 | `main.go:646-648` | کف پاکت |
| `per_link` پیش‌فرض | 8 | `main.go:653-655` | یک پیوند برای هر ۸ جریان فعال |
| `legacyMaxLinks` | 32 | `main.go:686` | سقف وقتی `max_links` غایب است |
| `icmpMaxLinks` | 8 | `main.go:678` | سقف dgtun روی icmp |
| `warmStartLinks` (موتور) | 8 | `engine/health.go:85` | `WarmSize` = گیرهٔ ۸ در `[min,max]` |
| `drainIdleDefault` (موتور) | 310 s | `engine/linkmanager.go:82` | وقتی `drain_idle_sec` غایب است |
| حد نرم حافظهٔ Go | RAM/2 | `main.go:317` | مگر `GOMEMLIMIT` |
| `statusInterval` | 2 s | `status.go:29` | دورهٔ نوشتن وضعیت و بازخورد |
| `warmMaxAge` | 15 min | `status.go:201` | فایل گرم کهنه‌تر نادیده |
| `warmAfter` | 1 min | `status.go:236` | تا یک دقیقه پس از شروع فایل گرم نوشته نمی‌شود |
| بازنویسی گرم | دست‌کم هر 1 min یا با تغییر هدف | `status.go:239` | |
| تازگی وضعیت برای `hs2 ports` | ≤ 7 s | `ports.go:256` | |
| کهنگی وضعیت در `hs2 status` و doctor | > 6 s | `status.go:456`، `doctor.go:131`، `:343`، `:464`، `:630`، `:662` | |
| `clkTck` | 100 | `status.go:365` | USER_HZ |
| cpuMeter: داغ | ≥ 90%×cores، 3 نمونهٔ پیاپی | `status.go:391-395` | لاگ «CPU گلوگاه است» |
| cpuMeter: آزاد | < 70%×cores | `status.go:396-401` | |
| `hostSatBusy` / `hostSatPSI` / `hostSatRuns` | 90% / 40% / 3 | `hostcpu.go:59-61` | اشباع سرور |
| `hostClearBusy` / `hostClearPSI` / `hostClearRuns` | 75% / 20% / 5 | `hostcpu.go:62-64` | پایان اشباع |
| tcpMemWatch: بالا / پایین | `mem ≥ tcp_mem[1]` / `< 0.9×tcp_mem[1]` | `status.go:847-853` | |
| watchCerts | هر 1 min | `cert.go:119` | پایش mtime |
| هشدار انقضای گواهی (اجرا/doctor) | ≤ 7 روز | `cert.go:136`، `doctor.go:206`، `status.go:539` | |
| هشدار انقضای گواهی (`check`) | < 14 روز | `check.go:288` | |
| مهلت اتصال TLS پیوند | 8 s | `tlscarrier/carrier.go:162` | `DialFrom` |
| مهلت اسکات reverse | 2 s | `main.go:534-536` | `RevDialScout` |
| مهلت اتصال endpoint در doctor | 8 s | `doctor.go:160` | |
| `ReadHeaderTimeout` سرور پوششی | 10 s | `main.go:990` | |
| `Cache-Control` صفحهٔ پوششی | `max-age=3600` | `main.go:981` | |
| فاصلهٔ Last-Modified | 18–400 روز (قدیمی: 37) | `cover.go:449`، `:252` | |
| `maxWireLinks` | 65535 | `check.go:453` | عدد پیوند u16 روی سیم |
| `maxSaneLinks` | 1024 | `check.go:454` | بالاتر = هشدار |
| `minLinksHigh` | 64 | `check.go:477` | `min_links` بالاتر = هشدار |
| هشدار `drain_idle_sec` | 0 < d < 300 | `check.go:249` | زیر connIdle پیش‌فرض xray |
| بازهٔ MTU غیرعادی | <576 یا >9000 | `check.go:344` | هشدار |
| `manyLinksAt` | 64 | `doctor.go:452` | یادداشت دیده‌شدن الگو |
| conntrack | ≥ 70% | `doctor.go:587` | هشدار |
| تونل‌های هم‌زمان | > 40% RAM | `doctor.go:691` | هشدار |
| `tune.LinkWorstCaseMiB` | 12 MiB | `tune/tune.go:206` | بدترین حالت بافر یک پیوند |
| `tune.LinkRAMPerLinkMB` | 48 MB | `tune/tune.go:208` | یک پیوند برای هر ۴۸ MB |
| `tune.MaxLinksCap` | 300 | `tune/tune.go:210` | بیشینهٔ سقف خودکار |
| `maxLinksFewCores` | 128 | `tune/tune.go:212` | ۲ تا ۳ هسته |
| سقف‌های پروفایل | 32/48/64 | `tune/tune.go:199-201` | کف سقف خودکار |
| smux frame / stream / session | 16 KiB / 2 MiB / 8 MiB | `engine/mtcp_link.go:258-264` | قابل تغییر با `HS2_TUNE_SMUX_*` |
| `NotSentLowat` سوکت پیوند | 32 KiB | `tlscarrier/tune_linux.go:19` | قابل تغییر با `HS2_TUNE_NOTSENT` |
| `CongestionControl` | `"bbr"` | `tlscarrier/tune_linux.go:29` | بعد با طرح tune جایگزین می‌شود |
| ثابت‌های کانال L3 | صف 256، دسته 16 KiB، 60 ms، 2 s، 8 s، 30 s، 10 s، 12 s | `engine/l3_link.go:49-88` | بخش ۳.۳ |

نمونه‌های سقف خودکار (از `tune/tune.go:194-196` و آزمون‌ها): 1GB/1 هسته → 32؛ 2GB/2 → 48؛ 2GB/4 → 64؛ 4GB/2 → 85؛ 8GB/4 → 170؛ 16GB/4+ → 300؛ جفت تولیدی 17GB/20 و 22GB/12 → 300.

مقادیر هستهٔ `tune.Build` (برای ارجاع؛ `tune/tune.go:280-373`): پروفایل high: `rmem/wmem_max=32MiB`، backlog 16384، somaxconn 8192؛ medium: 16MiB/8192/4096؛ low: 8MiB/2048/1024؛ ثابت‌ها: `tcp_notsent_lowat=131072`، `tcp_slow_start_after_idle=0`، `tcp_mtu_probing=1`، `tcp_fin_timeout=20`، `tcp_tw_reuse=1`، `rp_filter=2`؛ `tcp_rmem="4096 131072 max"`، `tcp_wmem="4096 65536 max"`؛ congestion پیش‌فرض bbr (بازگشت به cubic) و qdisc پیش‌فرض fq_codel (بازگشت به fq).

---

## ۵. حلقه‌های کنترلی

### ۵.۱ حلقهٔ وضعیت — `status.go:250-349`
- ورودی: `StatsFn` موتور، `/proc/net/sockstat` و `/proc/sys/net/ipv4/tcp_mem`، `/proc/self/stat`، `/proc/stat`، `/proc/pressure/cpu`، `/proc/net/snmp`، رجیستری گواهی.
- شرط: تیکر ۲ ثانیه؛ یک نوشتن فوری در شروع.
- خروجی: فایل `/run/hs2/<name>.status.json`؛ فایل گرم؛ `engine.SetTCPMemPressure`؛ `udpcarrier.SetHostSaturated`؛ لاگ‌های cpu/host/TCP memory.
- اگر ساختن `/run/hs2` شکست بخورد، کل حلقه اجرا نمی‌شود (`status.go:252-254`).

### ۵.۲ cpuMeter — `status.go:367-404`
- ورودی: utime+stime از `/proc/self/stat`. خروجی: درصد یک هسته (یک رقم اعشار).
- حالت `logged`: سه نمونهٔ پیاپی ≥ ۹۰٪×هسته‌ها → لاگ گلوگاه؛ زیر ۷۰٪×هسته‌ها → لاگ بازگشت. بین دو آستانه، شمارنده دست نمی‌خورد (فقط زیر ۷۰٪ صفر می‌شود).

### ۵.۳ hostMeter — `hostcpu.go:187-242`
- داغ: `busy ≥ 90` یا `PSI10 ≥ 40`؛ سرد: `busy < 75` (یا نامعلوم) و `PSI10 < 20`؛ میانه: هر دو شمارنده صفر.
- ۳ نمونهٔ داغ پیاپی (≈۶ ثانیه) → `saturated=true` + لاگ؛ ۵ نمونهٔ سرد پیاپی (≈۱۰ ثانیه) → پاک + لاگ.
- نمونه‌ای که هیچ چیز اندازه نگرفته (بدون PSI و شمارندهٔ برگشتی) وضعیت را تغییر نمی‌دهد.
- OutDiscards از اولین نمونه شمرده می‌شود؛ فقط نمایش، هرگز هشدار (چون روی شنوندهٔ icmp پاسخ‌های عمداً دورریخته را هم می‌شمارد).

### ۵.۴ tcpMemWatch — `status.go:840-855`
- `mem ≥ press` → `above=true` + لاگ؛ `mem < 0.9×press` → `above=false` + لاگ؛ هر تیک `engine.SetTCPMemPressure(above)`.

### ۵.۵ warmWriter — `status.go:238-246`
- فقط برای پول لبه؛ نوشتن نمی‌کند اگر هدف < 1، یا هنوز یک دقیقه از شروع نگذشته، یا هدف تغییر نکرده و کمتر از یک دقیقه از نوشتن قبلی گذشته.

### ۵.۶ watchCerts — `cert.go:118-142`
- هر دقیقه mtime را مقایسه و در صورت تغییر بارگذاری مجدد؛ هشدار انقضا یک‌بار.

### ۵.۷ گیرندهٔ SIGHUP و نگهبان خاموشی — `main.go:380-402`

---

## ۶. حالت‌ها، گذارها، خطاها و بازیابی

### ۶.۱ مسیرهای کشنده در `run` (همه → systemd ری‌استارت پس از ۳ ثانیه)
- فایل پیکربندی ناخوانا یا JSON نامعتبر (`main.go:332-335`).
- `bind_local_ip` نامعتبر (`main.go:339-341`).
- حامل ناشناخته (`main.go:422-423`).
- باز نشدن TUN (`main.go:462-463`) — مثلاً نبود root یا ماژول tun.
- بارگذاری اول گواهی/کلید (`main.go:492-493`، `:541-542`).
- bind شدن شنوندهٔ تونل (`main.go:494-495`، `:543-544`).
- `port_map` نامعتبر روی خروجی (`main.go:513-514`).
- خطای برگشتی از `RunIran`/`RunKharej` (مثلاً bind پورت کاربر؛ `engine/stream_iran.go:141-144`).

### ۶.۲ حالت‌های گواهی
`cur` (atomic) فقط با بارگذاری موفق عوض می‌شود؛ شکست = لاگ و ماندن روی قبلی (`cert.go:73-78`؛ آزمون `TestCertReloaderHotSwap`).

### ۶.۳ ماشین حالت اشباع سرور
`saturated ∈ {false,true}` با شمارنده‌های `hot`/`cool` (بخش ۵.۳).

### ۶.۴ ماشین حالت فایل گرم
نبود / کهنه (>۱۵ دقیقه) / نامعتبر → 0؛ تازه ولی ≤ `WarmSize` → 0 (یعنی شروع پیش‌فرض ۸)؛ تازه و بزرگ‌تر → گیره در `[min,max]` و لاگ.

### ۶.۵ حالت‌های سقف پیوند (`linkCeiling`، `main.go:711-724`)
ترتیب ارزیابی:
1. `max_links > 0` → `fixed` (عدد اپراتور، هرگز تغییر نمی‌کند).
2. dgtun روی icmp (با `max_links` صفر یا غایب) → `icmp` = 8.
3. `max_links` صریحاً 0 → `auto` = `tune.RecommendedMaxLinks(ram, cpus)` (در هر شروع دوباره محاسبه).
4. غایب (یا `null`) → `default` = 32.
سپس `linkEnvelope`: اگر `max < min` → `max = min` و لاگ «raised from N to min_links».

### ۶.۶ سقف مؤثر (`effectiveCeiling`، `status.go:584-606`)
- direct: فقط سقف ایران (خروجی می‌پذیرد هر چه لبه شماره بگیرد).
- reverse: کمترینِ دو سقف (لبه هدف را به سقف خودش و خروجی به سقف خودش گیره می‌زند).

---

## ۷. قالب‌ها (cmd پروتکل سیمی تعریف نمی‌کند)

- **پروتکل سیمی** (kindInfo/peerInfo، pool-control، kindL3، برچسب پورت ۲ بایتی) در موتور است؛ cmd فقط مقدارهایش را از `PoolStats` نمایش می‌دهد.
- **فایل وضعیت** `/run/hs2/<etc-hs2-config.json>.status.json`: JSON تخت `liveStatus` (`status.go:37-177`)؛ فیلدها فقط اضافه می‌شوند تا خواننده‌های قدیمی بشکنند نه (`engine/linkmanager.go:2214-2215`). نصب‌کننده آن را با grep/sed می‌خواند (`status.go:35-36`). حقوق فایل 0644.
- **فایل گرم** `/run/hs2/<...>.warm`: یک عدد صحیح و خط‌جدید؛ نوشتن اتمی با `.tmp`؛ حقوق 0644 (`status.go:243`).
- **خط نسخه**: `hs2 v3 (...)  [build <tag> <rev12>[+] <YYYY-MM-DD>]` (`main.go:207-212`).
- **پیکربندی**: JSON؛ `hs2 config`/`hs2 ports` آن را با `json.MarshalIndent` روی یک map بازنویسی می‌کنند (کلیدها مرتب الفبایی، دندانهٔ دو فاصله، خط‌جدید پایانی، حقوق 0600) (`config.go:141-147`، `:222-228`).

---

## ۸. متن دقیق لاگ‌های مهم و معنی‌شان

| متن | محل | معنی |
|---|---|---|
| `config: bind_local_ip %q is not a valid IP address` | `main.go:340` | خروج کشنده |
| `egress: all tunnel connections will leave from %s` | `main.go:343` | IP مبدأ اتصال‌های تونل |
| `tuning: %s=%d` / `tuning: HS2_TUNE_CC=%q` | `main.go:263`، `:273` | بازنویسی آزمایشگاهی فعال است |
| `memory: Go soft limit %d MB (half of %d MB RAM; set GOMEMLIMIT to override)` | `main.go:319` | |
| `tuning: %s (not applied: %s)` | `main.go:355` | `not root` یا `HS2_NO_TUNE set` |
| `tuning: profile %s (RAM %s, %d cpu) — %s + %s, %d sysctls` و `tuning: note — ...` | `tune/tune.go:441-459` | طرح اعمال شد |
| `link pool: ceiling %d links — %s` (+ ` (raised from %d to min_links)`) | `main.go:748-751` | سقف و دلیلش (auto/fixed/default/icmp) |
| `link pool: coming up at %d links, the size it had before this restart (the autopilot resizes it from there)` | `main.go:291` | شروع گرم |
| `tun %s up: %s peer %s (side channel)` | `main.go:466` | TUN در l3mtcp/tls |
| `stream edge (reverse): listening for kharej links on %s` | `main.go:498` | |
| `stream exit (reverse): dynamic link pool %d–%d to edge %s (edge drives the count)` | `main.go:537` | |
| `tun %s up: %s peer %s mtu %d (datagram pool, encap %s, %s)` | `main.go:851` | dgtun |
| `user port %s open (%s), carried over the link pool` | `engine/stream_iran.go:172` | پورت کاربر روی پول |
| `l3: dropped %d packets in 30s on the tun side channel (queue limit or no link) — ...` | `engine/l3_link.go:389` | کانال جانبی TUN در l3mtcp زیر بار دور ریخت؛ پورت‌های کاربر اثر نمی‌پذیرند |
| `cert: reload (%s) failed, keeping the current certificate: %v` | `cert.go:76` | |
| `cert: reloaded (%s) — now valid until %s` / `— unchanged` | `cert.go:80`، `:82` | |
| `cert: %s expires in %d day(s) — renew it now; 'hs2 doctor' (cert renewal) shows whether and how it renews` | `cert.go:137` | |
| `cpu: hs2 is using %.0f%% of its %d core(s) — the CPU is the bottleneck now, not the path ...` | `status.go:394` | فرایند خودش گلوگاه است |
| `cpu: back to %.0f%% of %d core(s) — no longer the bottleneck` | `status.go:400` | |
| `cpu: the server is saturated — %s; hs2 itself uses %.0f%% of one core. Every carrier now sends late ...` | `hostcpu.go:230` | کل سرور اشباع است |
| `cpu: the server has room again — %s` | `hostcpu.go:236` | |
| `kernel TCP memory: %d MB in TCP buffers, above the kernel's pressure mark (...) ...` | `status.go:849` | فشار حافظهٔ TCP هسته |
| `kernel TCP memory: back below the pressure mark (%d MB of %d MB)` | `status.go:852` | |
| `pprof: HS2_PPROF=%q refused — ...` / `pprof: serving profiles on http://%s/debug/pprof/ (HS2_PPROF)` | `pprof.go:27`، `:41` | |

خروجی‌های غیرلاگ مهم: `check` → `WARN:  ` / `ERROR: ` / `config OK` (`check.go:45-54`)؛ doctor → `[ ok ]`/`[warn]`/`[fail]`/`[info]` و جمع‌بندی (`doctor.go:94-114`).

---

## ۹. گزینه‌های پیکربندی و متغیرهای محیطی

### ۹.۱ کلیدهای `fileConfig` (`main.go:36-105`)

| کلید | نوع | پیش‌فرض/رفتار خالی | چه کسی/کجا استفاده می‌کند | در l3mtcp |
|---|---|---|---|---|
| `mode` | رشته | — (`check` فقط `dial`/`listen` را می‌پذیرد) | نقش: `dial`=ایران/لبه، `listen`=خارج/خروجی | بله |
| `iface` | رشته | خالی | نام TUN (حداکثر ۱۵ نویسه در `check`) | بله |
| `local_cidr` | رشته | خالی | IP/پیشوند TUN | بله |
| `peer_ip` | رشته | خالی | IP طرف مقابل روی TUN | بله |
| `mtu` | عدد | 0 → 1380 در stream، 1280 در udp/auto/dgtun | MTU TUN | بله (نصب‌کننده: 1380 از منوی TCP، 1320 از منوی tun-over-TCP؛ install.sh:2103 و :1669) |
| `carrier` | رشته | خالی = noise | انتخاب حامل | `l3mtcp` یا `l3` |
| `addr` | رشته | — | مقصد شماره‌گیری یا آدرس گوش‌دادن | بله |
| `encap` | رشته | خالی = udp | فقط dgtun | خیر |
| `proto` | عدد | 0 (ipx: پیش‌فرض بسته encap) | فقط dgtun+ipx | خیر |
| `reverse` | بولی | false | چه کسی TLS را شماره می‌گیرد | بله |
| `bind_local_ip` | رشته | خالی | IP مبدأ شماره‌گیری (نامعتبر = خروج) | بله (طرف شماره‌گیر) |
| `user_listen_ip` | رشته | خالی (همهٔ IPها) | IP شنوندهٔ پورت‌های کاربر (لبه) | بله |
| `udp` | بولی | false | ارسال UDP هم روی پورت‌های کاربر (لبه) | بله |
| `min_links` | عدد | ≤0 → 2 | کف پول | بله |
| `max_links` | عدد | غایب → 32، `0` → خودکار، مثبت → ثابت | سقف پول | بله |
| `per_link` | عدد | ≤0 → 8 | جریان فعال برای هر پیوند | بله (فقط لبه اثر دارد) |
| `drain_idle_sec` | عدد اختیاری | غایب → 310 s موتور، `0` → هرگز، n → n ثانیه | بستن اتصال بی‌کار روی پیوند در حال بازنشستگی | بله (لبه) |
| `expose` | رشته | خالی | پنل پیش‌فرض خروجی | بله (خروجی) |
| `forward_ports` | رشته CSV | خالی | پورت‌های کاربر روی لبه | بله؛ خالی = هشدار «تونل مسیریابی خالص» |
| `peer_panel` | رشته | — | فقط اطلاعاتی (در کد استفاده نمی‌شود) | — |
| `port_map` | رشته | خالی | مقصد اختصاصی هر پورت روی خروجی (`P` یا `P=host:port`) | بله (خروجی) |
| `sni` | رشته | خالی (هشدار DPI در `check`) | دامنهٔ دست‌دهی TLS طرف شماره‌گیر | بله |
| `cover_addr` | رشته | — | فقط reality | خیر |
| `backend_addr` | رشته | خالی/`builtin` → وب‌سایت داخلی | پشتیبان پروب طرف سرور TLS | بله |
| `cover_seed` | رشته | خالی → صفحهٔ ثابت قدیمی | بذر صفحهٔ پوششی (از `shared_key` مشتق نمی‌شود) | بله |
| `shared_key` | هگز | — (۳۲ بایت انتظار می‌رود) | کلید مشترک دو طرف | بله |
| `cert_file`/`key_file` | مسیر | — | فقط طرف سرور TLS (خروجی direct یا لبهٔ reverse) | بله |
| `local_priv`/`local_pub`/`remote_static`/`psk` | هگز | — | فقط noise | خیر |
| `tuning` | شیء | غایب = auto | `mode` (auto/manual/off)، `congestion`، `qdisc`، `rmem_max`، `wmem_max`، `netdev_backlog`، `somaxconn` (`tune/tune.go:41-52`) | بله |

کلیدهای قابل ویرایش با `hs2 config` (`config.go:39-59`): `min_links`، `max_links` (مقدار `auto` → 0)، `per_link`، `drain_idle_sec`، `forward_ports`، `port_map`، `expose`، `udp`، `tuning.mode`، `tuning.congestion`، `tuning.qdisc`، `tuning.rmem_max`، `tuning.wmem_max`، `tuning.netdev_backlog`، `tuning.somaxconn`. هر تغییر پیش از ذخیره با `checkConfig` سنجیده می‌شود؛ اثر پس از ری‌استارت.

پیش‌فرض‌های نصب‌کننده: `LINK_MIN=2`، `LINK_MAX=32` که در نصب جدید به `0` (خودکار) تبدیل می‌شود، `LINK_PER=8` (install.sh:52-57، :1780).

### ۹.۲ متغیرهای محیطی مرتبط با cmd

| متغیر | محل | اثر |
|---|---|---|
| `HS2_TUNE_NOTSENT` | `main.go:267` | `tlscarrier.NotSentLowat` (پیش‌فرض 32 KiB) |
| `HS2_TUNE_SMUX_FRAME` | `main.go:268` | `engine.SmuxFrameSize` (16 KiB) |
| `HS2_TUNE_SMUX_STREAMBUF` | `main.go:269` | `engine.SmuxStreamBuffer` (2 MiB) |
| `HS2_TUNE_SMUX_SESSBUF` | `main.go:270` | `engine.SmuxSessionBuffer` (8 MiB) |
| `HS2_TUNE_CC` | `main.go:271-274`، `:357` | کنترل ازدحام سوکت پیوندها؛ اگر تنظیم شود طرح tune روی آن نمی‌نشیند |
| `HS2_NO_TUNE` | `main.go:352` | sysctlهای سیستم اعمال نمی‌شوند |
| `GOMEMLIMIT` | `main.go:310` | جایگزین حد نرم نصف RAM |
| `HS2_PPROF` | `pprof.go:21` | سرور pprof فقط روی loopback |
| `HS2_TUN_OFFLOAD` | `main.go:840` | `0` = خاموش کردن offload در dgtun (روی l3mtcp اثری ندارد) |

متغیرهای دیگر موتور/حامل (`HS2_DG_FQ`، `HS2_FAIR_SHARE`، `HS2_DG_PAD`، `HS2_ICMP_CAMO`، `HS2_RAW_BATCH`، `HS2_RAW_TX`، `HS2_ICMP_SUPPRESS`، `HS2_TUN_RCVBUF`، `HS2_TUN_REORDER_MS`) در cmd خوانده نمی‌شوند و همه مربوط به مسیر دیتاگرامی‌اند.

---

## ۱۰. آزمون‌ها: چه چیزی تضمین می‌شود

**سقف و پاکت پیوند (`linkpool_test.go`)**
- `TestLinkCeilingAutoFollowsHardware`: خودکار برای 1GB/1→32، 2GB/2→48، 8GB/4→170، 17GB/20 و 22GB/12→300.
- `TestLinkCeilingFixedIsNeverMoved`، `TestLinkCeilingAbsentKeepsHistorical32` (غایب و `null` → 32).
- `TestLinkEnvelopeMinAboveAuto`: `min_links` سقف را بالا می‌برد و لاگ می‌گوید.
- `TestCeilingLogLine`، `TestEffectiveCeiling`، `TestCeilingLineBothServers`، `TestCeilingLiftedByMinLinks`، `TestDriftLine`، `TestPatternLineCappedByKharej`.
- `TestDoctorLinkPool`، `TestDoctorManyLinks`، `TestVisibilityNotePerCarrier`، `TestCheckWarnsHighMinLinks`.
- `TestStatusFileCarriesCeiling`: فیلدهای سقف در فایل وضعیت و نوشته شدن فایل گرم.
- `TestConfigSetMaxLinksAuto`: `auto` → 0؛ فقط برای `max_links`.
- `TestWarmFile`: یک دقیقهٔ اول نوشته نمی‌شود؛ فقط بالا می‌برد؛ کهنه و خراب = 0.
- `TestDgtunAutoCeilingIsTheStreamPools`، `TestICMPTunCeiling`، `TestDoctorICMPCeiling`.
- `TestDoctorTCPMem`، `TestDoctorConntrackAndTCPMemWatch` (پسماند ۹۰٪ و رسیدن سیگنال به موتور).
- `TestTunnelsTogetherCountsEffectiveCeilings`، `TestStatusNamesTheCoresTheCeilingUses`.

**اعتبارسنجی (`check_test.go`)**
- `TestCheckAcceptsInstallerConfigs`: همهٔ شکل‌های نصب‌کننده (شامل چهار شکل l3mtcp) بدون خطا و هشدار.
- `TestCheckCatchesMistakes`، `TestCheckCertificate` (گواهی منقضی = هشدار، برخورد پورت کاربر با پورت تونل = خطا)، `TestCheckDrainIdle`، `TestCheckL3TunPortsAndPanel` (l3mtcp بدون پورت/پنل = هشدار نه خطا؛ mtcp بدون پورت = خطا)، `TestCheckDgTun`، `TestCheckPoolBounds`.

**پورت‌ها (`ports_test.go`)**: `TestCheckExitTable`، `TestPortLinesIran`، `TestPortLinesKharej`، `TestPortsEditIran`، `TestPortsEditKharej` (تغییر رد شده فایل را دست نمی‌زند؛ `port_map` خراب بازنویسی نمی‌شود)، `TestConfigSetPortKeys`، `TestLocalListening`، `TestStatusShowsPorts`، `TestDoctorPorts`.

**وضعیت (`status_test.go`، `kharej_stats_test.go`)**: `TestStatusPathMatchesInstaller` (قرارداد مسیر با bash نصب‌کننده)، `TestLiveStatusRoundTrip`، `TestLiveStatusShrinkingPool`، `TestKharejStatusCarriesItsOwnCounts`، `TestRefillNoteInStatusAndDoctor`.

**CPU (`cpu_test.go`، `hostcpu_test.go`)**: `TestCPUMeter`، `TestHostProcParsers`، `TestHostMeterSaturation` (پسماند ۳/۵ و PSI)، `TestHostMeterDiscards`، `TestDoctorCPU`، `TestStatusSendStageAndHostLines` (و اینکه شمارندهٔ مرده `pacer_dropped` دیگر نوشته نمی‌شود).

**گواهی (`cert_test.go`، `doctor_test.go`، `doctor_cert_test.go`)**: `TestCertReloaderHotSwap`، `TestCheckCert` (فقط طرف سرور TLS)، `TestCheckEndpointGating`، `TestNormalizeWS`، `TestAddrsContainCIDR`، `TestDoctorReportTally`، `TestCertbotLineage`، `TestReadRenewalConf`، `TestCheckCertRenewal`، `TestCheckCertReturnsLeafForRenewal`، `TestListeningOnPort`، `TestCheckCertRenewalHooksAndLifetime`، `TestRenewThresholdDays`، `TestCronRunsCertbot`.

**پوشش (`main_test.go`، `cover_test.go`)**: `TestBuiltinBackendServesCover` (بدون سرآیند Server، بدون nginx)، `TestBuiltinBackendConditional304`، `TestBuiltinBackend404`، `TestCoverDeterministic` (هش طلایی `e7cc8932…`)، `TestCoverSeedsDiffer`، `TestCoverSeedlessIsLegacy` (هش `86dfcf86…`)، `TestCoverStructuralInvariants`، `TestCoverServedHeaders`، `TestCoverETagSaltedAndLegacyNone`، `TestCoverNeutralsVary`، `TestCoverBackendBothModes`.

**دیگر**: `TestPprofLoopbackOnly`.

**آنچه آزمون ندارد** (مشاهده): `runCmd`/`runStream`/`runDgTun` به‌صورت واحد آزموده نمی‌شوند (نیاز به TUN و root)؛ `applyTuning`، `setMemoryLimit`، `warmLinks` با گیره، گیرندهٔ SIGHUP و نگهبان خاموشی آزمون مستقیم ندارند (`warmLinks` در `TestWarmFile` پوشش جزئی دارد).

---

## ۱۱. «از قبل وجود دارد» (برای جلوگیری از دوباره‌کاری)

1. پاکت تطبیقی پیوند: `min_links`/`max_links`/`per_link` با پیش‌فرض‌ها؛ سقف خودکار از RAM و هسته (آگاه از cgroup و GOMAXPROCS) تا ۳۰۰؛ سقف ثابت؛ ۳۲ تاریخی؛ ۸ برای icmp؛ بالا بردن سقف تا `min_links`.
2. شروع گرم پس از ری‌استارت (۱۵ دقیقه، فقط افزایشی، نوشتن پس از یک دقیقه، ضد حلقهٔ کرش).
3. حالت reverse برای همهٔ حامل‌های stream؛ پول پویا در خروجی reverse که لبه هدایتش می‌کند؛ اسلات اسکات با اتصال ۲ ثانیه‌ای.
4. سقف مؤثر دو طرف (direct: ایران؛ reverse: کمترین) و تبادل سقف بین دو سرور؛ نمایش در status/doctor/منو.
5. تنظیم هسته در هر شروع (bbr، fq_codel، بافرها بر اساس پروفایل، `tcp_tw_reuse`، `rp_filter=2`، `mtu_probing`، `slow_start_after_idle=0`، `notsent_lowat`) با حالت‌های auto/manual/off و `HS2_NO_TUNE`؛ بازگرداندن `ip_local_port_range` که بیلد قبلی پهن کرده بود.
6. کنترل ازدحام سوکت پیوندها هم‌راستا با طرح tune؛ `NotSentLowat` ۳۲ KiB روی سوکت‌های پیوند.
7. حد نرم حافظهٔ Go = نصف RAM.
8. بارگذاری داغ گواهی (SIGHUP + پایش mtime)، هشدار انقضا، و تحلیل کامل تمدید certbot در doctor.
9. وب‌سایت پوششی داخلی با بذر هر نصب، ETag نمک‌زده، بدون سرآیند Server، 404 برای بقیه، رفتار سرور ایستا؛ گزینهٔ `backend_addr` برای سایت واقعی.
10. مسیریابی پورت به پورت (`forward_ports` روی ایران، `port_map`/`expose` روی خارج) و دستور `hs2 ports` و نمایش جدول طرف مقابل.
11. ارسال UDP روی پورت‌های کاربر؛ `bind_local_ip`؛ `user_listen_ip`.
12. فایل وضعیت زنده + `hs2 status [--watch]` + منوی نصب‌کننده؛ دلیل اندازهٔ پول، serving/retiring، refill، exit stats.
13. متر CPU خود فرایند و متر کل سرور (busy/softirq/steal/PSI/OutDiscards) با پسماند؛ رساندن اشباع سرور به پیس‌کنندهٔ udpcarrier.
14. پایش فشار حافظهٔ TCP هسته و رساندنش به محافظ و قواعد سلامت موتور.
15. doctor جامع (پیکربندی، اجرا، endpoint، گواهی، تمدید، TUN، تنظیم، سقف، دیده‌شدن، پورت‌ها و گوش‌دادن مقصدها، حافظهٔ TCP، conntrack، CPU، چند تونل، ساعت).
16. `check` با تشخیص کلید ناشناخته، خط و ستون خطای JSON، بررسی IP محلی، برخورد پورت، هشدار SNI خالی و …؛ `config` با لیست سفید و اعتبارسنجی پیش از ذخیره و نوشتن اتمی.
17. `cleanup` قواعد echo guard؛ `recommend-links`؛ `tune`؛ pprof فقط loopback.
18. نگهبان خاموشی ۳ ثانیه‌ای؛ شنونده‌های SO_REUSEADDR و بدون MPTCP؛ مهر ساخت در نسخه.
19. بازنویسی آزمایشگاهی اندازه‌های smux و NotSentLowat و CC با متغیر محیطی.
20. (موتور، برای l3mtcp) کانال جانبی TUN روی یک جریان smux در هر پیوند، درهم‌سازی rendezvous، حد ماندن ۶۰ ms، keepalive آرام ۱۰ ثانیه، جابه‌جایی ۱۲ ثانیه‌ای جریان‌ها از جلسهٔ ساکت.
21. (dgtun) offload TCP در TUN.

---

## ۱۲. ایده‌هایی که امتحان و رد/جایگزین شده‌اند (طبق کد و مستندات)

| ایده | سرنوشت و دلیل | منبع |
|---|---|---|
| صفحهٔ پوششی ثابت برای همه | با یک هش همهٔ سرورها پیدا می‌شدند؛ جایگزین با صفحهٔ بذری | `cover.go:12-41` |
| ETag برابر هش خام بدنه | اثرانگشت تک‌پروبی؛ رد شد، نمک با بذر | `main.go:960-966`، `TestCoverETagSaltedAndLegacyNone` |
| سرآیند `Server: nginx` و صفحهٔ خوش‌آمد nginx | تناقض با TLS گو و امضای honeypot؛ حذف | `main.go:973-975`، `main_test.go:11-14` |
| `math/rand` برای مولد پوشش | جریانش بین نسخه‌های Go فرق می‌کند؛ SHA-256 | `cover.go:43-46` |
| تغییر مولد پوشش در جا | ممنوع (رویداد هم‌زمان کل ناوگان)؛ آزمون طلایی | `cover.go:34-41`، `cover_test.go:33-41` |
| سقف ثابت ۶۴ پیوند | جایگزین با قاعدهٔ RAM تا ۳۰۰ | ریشه CHANGELOG.md:720-737، `doctor.go:450-452` |
| سقف موقت dgtun (۶۴ روی encap خام، ۱۲۸ روی udp) | پس از آزمون بار ۳۰۰ حامل برداشته شد | `linkpool_test.go:494-498` |
| نوشتن فوری فایل گرم | حلقهٔ کرش مقدار بالا را تمدید می‌کرد؛ تأخیر یک‌دقیقه‌ای | `status.go:232-236` |
| کاهش اندازهٔ شروع با فایل گرم | رد؛ فقط افزایشی (هجوم صبحگاهی) | `main.go:285-286` |
| نادیده گرفتن بی‌صدای `bind_local_ip` غلط | تونل مرموزانه مرده به نظر می‌رسید؛ اکنون خروج | `main.go:336-338` |
| شمردن `max_links` خود هر تونل در «tunnels together» | بازبینی: روی خارج direct عدد اعمال‌نشده؛ اکنون سقف مؤثر | `linkpool_test.go:623-626` |
| نمایش هسته‌های میزبان به‌جای هسته‌های cgroup | بازبینی؛ اصلاح | `linkpool_test.go:663-665` |
| شمارندهٔ `pacer_dropped` | مرده؛ حذف از وضعیت | `hostcpu_test.go:280-282` |
| پهن کردن `ip_local_port_range` | با پورت‌های پنل برخورد می‌کرد؛ رد و خنثی‌سازی | `tune/tune.go:363-366`، `:405-407` |
| شنوندهٔ MPTCP (پیش‌فرض Go 1.24+) | `notsent_lowat` را نادیده می‌گرفت؛ p99 هشت ثانیه؛ خاموش | `engine/listen.go:22-28`، `go.mod:5-7` |
| حمل IP درون TCP در tls/l3mtcp (v2) | صف چندثانیه‌ای؛ اکنون stream core و TUN فقط کانال جانبی | README.md:659-666 |
| پینگ کنترلی با jitter | هشدار کاذب از دست رفت دانلود را برگرداند؛ ثابت ۳ ثانیه | ریشه CHANGELOG.md:767-770 |
| صف ارسال عمیق‌تر زیر اشباع CPU | امتحان شد، اثری نداشت | ریشه CHANGELOG.md:1550-1551 |
| مهلت ۴ ثانیه‌ای اتصال endpoint در doctor | روی مسیر پرتلفات ایران کم بود؛ ۸ ثانیه | `doctor.go:155-159` |
| اجرای `certbot --dry-run` یا `modprobe` در doctor | فقط‌خواندنی نیست؛ رد | `doctor_cert.go:26-28`، `doctor.go:406-414` |
| بررسی گواهی در طرف شماره‌گیر | سخت‌گیرتر از دیمن؛ رد («Finding 2») | `doctor_test.go:96-97` |
| آستانهٔ ثابت ۳۰ روز برای تمدید | جایگزین با قاعدهٔ certbot (یک‌سوم عمر) | `doctor_cert.go:62-66` |
| پذیرفتن بی‌صدای `min_links > max_links` در خارج | اکنون خطا | ریشه CHANGELOG.md:740-742 |

---

## ۱۳. محدودیت‌ها و مشاهده‌ها (بدون پیشنهاد تغییر)

1. **مشاهده — `run` اعتبارسنجی نمی‌کند**: `runCmd` فقط `bind_local_ip` را بررسی می‌کند؛ `mode` غلط (نه `dial` و نه `listen`) بی‌صدا «خروجی» تعبیر می‌شود (`main.go:468`)، و `unhex` خطای هگز را نادیده می‌گیرد و کلید ناقص/خالی می‌دهد (`main.go:1005`). نصب‌کننده `check` را اجرا می‌کند، ولی ویرایش دستی بدون `check` به این مسیر می‌رسد.
2. **مشاهده — TUN در l3mtcp بدون offload**: `tun.Open` بدون `Options{Offload}` (`main.go:462`) در حالی که dgtun offload دارد (`main.go:840-841`). با توجه به اینکه TUN در l3mtcp طبق طراحی فقط کانال جانبی با حد ۶۰ ms است، این احتمالاً عمدی است؛ نامطمئنم که سنجیده شده باشد.
3. **مشاهده — وابستگی سیگنال‌های مسیر داده به فایل وضعیت**: `SetTCPMemPressure` و `SetHostSaturated` فقط از حلقهٔ وضعیت صدا زده می‌شوند؛ اگر `/run/hs2` ساخته نشود هیچ‌کدام (و فایل گرم) کار نمی‌کنند (`status.go:252-254`). همچنین تأخیرشان به دورهٔ ۲ ثانیه و پسماند ۳/۵ نمونه بسته است.
4. **مشاهده — `hs2 doctor` برای mtcp احتمالاً هشدار کاذب TUN می‌دهد**: `checkTun` فقط با `iface` خالی رد می‌شود (`doctor.go:218-221`) و توضیحش می‌گوید mtcp iface ندارد، اما نصب‌کننده برای همهٔ حامل‌های منوی TCP (از جمله mtcp) `iface` و `mtu 1380` می‌نویسد (install.sh:1875، :2103) و `runStream(false)` TUN نمی‌سازد؛ پس `tun hs0: not present` بیرون می‌آید.
5. **مشاهده — MTU متفاوت l3mtcp بسته به منو**: منوی «TCP → l3mtcp» عدد 1380 و منوی «tun over TCP» پیش‌فرض 1320 می‌نویسد (install.sh:2103 در برابر :1669 و :2276).
6. **مشاهده — توضیح کهنه در `linkCeiling`**: `main.go:692` هنوز «low 32, medium 48, high 64» می‌گوید در حالی که قاعدهٔ واقعی تا ۳۰۰ است (`tune/tune.go:177-196`).
7. **مشاهده — توضیح ناهمخوان در موتور**: `IranConfig` می‌گوید در reverse «Dialer/Min/Max are ignored» (`engine/stream_iran.go:28-29`) اما `RunIran` از `cfg.Min/Max` برای پول استفاده می‌کند (`:69`).
8. **مشاهده — گواهی reality داغ بارگذاری نمی‌شود** (`main.go:439`).
9. **مشاهده — پیام خطای حامل ناشناخته** فهرست ناقص دارد (`check.go:142`: noise/reality/l3 نیامده‌اند)؛ `baseVersion` هم noise/reality را نام نمی‌برد (`main.go:127`).
10. **مشاهده — `check` کلیدهای ناشناختهٔ زیر `tuning` را گزارش نمی‌کند** (فقط سطح بالا؛ `check.go:118-131`).
11. **مشاهده — بازنویسی کل فایل در `hs2 config`/`hs2 ports`**: ترتیب کلیدها الفبایی می‌شود (map)؛ نوشتن اتمی بدون fsync (`config.go:141-147`، `:222-228`).
12. **مشاهده — حد حافظه و سقف پیوند هر تونل از کل سرور حساب می‌شود**: چند تونل روی یک سرور هر کدام `GOMEMLIMIT` نصف RAM می‌گیرند؛ doctor فقط جمع بافر پیوندها را هشدار می‌دهد نه جمع حد حافظه (`doctor.go:649-697`).
13. **مشاهده — خطای bind پورت کاربر کل دیمن را می‌کشد** (`engine/stream_iran.go:141-144` → `must`) و systemd هر ۳ ثانیه دوباره امتحان می‌کند؛ در این حلقه فایل گرم تازه نمی‌شود (عمداً).
14. **مشاهده — خروجی reverse همیشه از `WarmSize` (۸) شروع می‌کند** و تا رسیدن پیام pool-control لبه همان را نگه می‌دارد (`main.go:530`).
15. **مشاهده — `watchCerts` یک پرچم `warned` مشترک برای همهٔ reloaderها دارد** (`cert.go:121`)؛ در عمل هر فرایند یک reloader دارد، پس بی‌اثر است.
16. **مشاهده — `dialing` و `engineDialsTransport` یک فرمول‌اند** (`main.go:430`، `:907`).
17. **مشاهده — `HS2_TUNE_SMUX_*` فقط محلی است**: توضیح موتور می‌گوید تنظیمات smux «shared by both ends» است (`engine/mtcp_link.go:253`)؛ نامطمئنم که ناهمخوانی اندازهٔ قاب بین دو طرف مشکل‌ساز باشد یا نه.
18. **مشاهده — خروج نگهبان خاموشی همیشه کد ۰ است** (`main.go:384`)، حتی اگر موتور در حال گیر کردن بوده باشد.
19. **مشاهده — `per_link` روی خروجی reverse اثری ندارد** (فقط لبه اندازه می‌گیرد؛ `linkpool_test.go:526-552` همین را برای متن doctor تضمین می‌کند).

---

## ۱۴. ارجاع به زیرسیستم‌های دیگر

**cmd صدا می‌زند:**
- `engine`: `New`، `RunDial`/`RunListen` (noise/udp/auto/reality)، `RunIran`، `RunKharej`، `IranConfig`، `KharejConfig`، `NewMTCPDialer`، `ListenReuse`، `WarmSize`، `ParsePortMap`، `FormatPortMap`، `RouteTable`، `PeerRoutes`، `PoolStats`، `StatsFn`، `SetTCPMemPressure`/`TCPMemPressure`، `DgTunPort`(28443)/`DgTagPort`(28444)، `RunDgEdge`/`RunDgExit`، `StartDgPorts`، `DgPortsConfig`، `DgConfig`، `EncapConfig`، `NewDgDialer`/`NewDgListener`، `NewRealityDialer`/`NewRealityListener`، `NewNoiseDialer`/`NewNoiseListener`، `NewUDPDialer`/`NewUDPListener`، `NewAutoDialer`/`NewAutoListener`، `SmuxFrameSize`/`SmuxStreamBuffer`/`SmuxSessionBuffer`.
- `tlscarrier`: `Server{SharedKey, GetCertificate, BackendAddr, Logf}`، `Carrier`، `DialFrom`، `DialFromTimeout`، `NotSentLowat`، `CongestionControl`.
- `tun`: `Open`، `OpenWith`، `Options{Offload}`، `Device.Name/Close/Offloaded/OffloadStats`.
- `tune`: `Config`، `Build`، `Plan.Apply/Summary/Report`، `Detect`، `AvailableCC`، `AvailableQdisc`، `RecommendedMaxLinks`، `MaxLinksReason`، `ProfileFor`، `LinkWorstCaseMiB`.
- `encap`: `ReleaseAllEchoGuards`، `SweepStaleEchoGuards`، `IsHs2Daemon`، `SendRefused`، `DefaultIPXProto`.
- `udpcarrier`: `SetHostSaturated`.
- `core`: `GenerateStatic`، `StaticKey`.

**cmd صدا زده می‌شود از / خوانده می‌شود توسط:**
- systemd (`hs2 run`، SIGHUP، SIGTERM).
- نصب‌کننده (`install.sh`): `hs2 version` (دروازهٔ `hs2_is_v3`)، `hs2 check`، `hs2 config`، `hs2 ports`، `hs2 recommend-links [-c] [--why]`، `hs2 cleanup`، و خواندن مستقیم JSON فایل وضعیت در `/run/hs2`.
- موتور، به‌صورت callback: `OnStart(StatsFn)` → `startStatusWriter`؛ `tlscarrier.Server.GetCertificate` → `certReloader.getCertificate`؛ `BackendAddr` → سرور پوششی داخلی.
