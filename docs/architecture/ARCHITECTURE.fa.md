# معماری hs2 — سند یکپارچه (تمرکز روی تونل اصلی l3mtcp)

> **واقعیت استقرار (تأیید صاحب پروژه، ۲۰۲۶-۱۰-۱۰):** کاربران از `forward_ports` می‌آیند، نه از hs0. hs0 فقط کانال جانبی پینگ و ترافیک سبک است. پس مسیر اصلی داده «اتصال TCP کاربر ← جریان smux ← لینک mtcp» است (بخش‌های ۲.۶، ۳، ۴، ۵، ۶) و شکاف‌های ویژهٔ hs0 (بخش ۱۲.۱) اولویت پایینی دارند.
>
> **به‌روزرسانی پس از فاز Z (۲۰۲۶-۱۰-۱۰، شاخهٔ `claude/smux-buffer-constants-fv9bu8`):** این سند وضعیت ثبت `0812bc9` را می‌گوید. فاز Z دو شکاف مسیر پورت‌های کاربر را بست (جزئیات و عددها در `CHANGELOG.md`، بخش Phase Z):
> - **خوانندهٔ کند ولی زنده (F13، بخش ۷.۴):** smux با وصلهٔ «پنجرهٔ دریافت تطبیقی هر جریان» در `hs2-src/third_party/smux` (جریان با ۲۵۶KiB شروع می‌کند، تا ۶۴KiB کوچک و تا ۲MiB بزرگ می‌شود؛ `SmuxMinStreamBuffer`/`SmuxStreamLagTarget` در `engine/mtcp_link.go`). باقی‌مانده: خواننده‌ای که پس از تند خواندن کند شود، لینک را حدود ۷ ثانیه می‌خواباند؛ شروع جریان بزرگ ۱ تا ۲ RTT دیرتر به پنجرهٔ کامل می‌رسد.
> - **جذب کاربر تازه به لینک در حال مرگ (بخش ۷.۵ بند ۳ و ۵):** علامت `lagging` در `engine/linkmanager.go` (پینگ ≥۲s، نوشتن ≥۲s، سکوت ≥۱۰s، بی‌پاسخی جریان تازه ≥۱s یا ۲×RTT، باز کردن کند اخیر) با قاعدهٔ اکثریت در `pickLocked`؛ `openStream` با مهلت ۳s برای هر تلاش و تلاش موازی فقط وقتی SYN نرفته (`engine/stream_iran.go`). باقی‌مانده: کاربری که در ثانیهٔ نخست پس از سیاه‌چاله شدن لینک روی آن بنشیند هنوز تا مرگ لینک (~۲۰s) منتظر می‌ماند.
>
> **مبنا:** مخزن `/home/user/hs2-`، شاخهٔ برابر `main`، ثبت `0812bc9`. کد Go باینری منتشرشده (`hs2-linux-amd64`، build `da621f5db2bb`، `go1.27.0`) با همین ثبت یکسان است؛ `git diff da621f5 0812bc9` فقط باینری، هش آن و `hs2-src/VALIDATION.md` را نشان می‌دهد. پس هر path:line این سند همان رفتاری است که سرورهای میدانی اجرا می‌کنند.
>
> **قرارداد مسیرها:** مسیر بی‌پیشوند نسبت به `/home/user/hs2-/hs2-src` است (مثلاً `engine/l3_link.go:62`). `install.sh`، `README.md` و `CHANGELOG.md` بی‌پیشوند یعنی فایل‌های ریشهٔ مخزن؛ README درونی با `hs2-src/README.md` آمده است. `smux@` یعنی `/root/go/pkg/mod/github.com/xtaci/smux@v1.5.24`.
>
> **برچسب‌ها:** «واقعیت» = مستقیم از کد. «سنجیده» = با آزمون اجراشده تأیید شده (در نقشه‌های جزئی). «استنتاج» = از ترکیب کد درآمده ولی اجرا نشده. «محاسبه» = عدد از فرمول‌های کد حساب شده. «نامطمئن» = به هستهٔ لینوکس یا شبکه بستگی دارد یا کامل وارسی نشده.
>
> **منبع:** این سند از ۱۷ نقشهٔ جزئی (فهرست در پیوست) ساخته شده و اصلاحیه‌های راستی‌آزما و منتقد (نقشه‌های ۹۰ و ۹۱ و یادداشت‌های ۲۰ و ۲۱) در آن **اعمال شده‌اند**. جاهایی که نقشه‌ها با هم اختلاف داشتند، داوری با کد انجام شده و چند مورد هنگام نگارش دوباره در کد دیده شد (`engine/l3_link.go:49-87`، `engine/control.go:102-135`، `engine/linkmanager.go:1735-1744, 1062-1064, 1874-1890, 2196-2208`، `engine/wedge.go:136-160`، `engine/stream_iran.go:99-128`، `tlscarrier/carrier.go:183-190`، `engine/exit_pool.go:528-536`، `engine/stream.go:36-48`، `install.sh:1664-1672` و همهٔ سطرهای `"mtu"` نصب‌کننده). هیچ فایلی در مخزن تغییر نکرد.

---

## ۰. فهرست مطالب

1. نمای کلی: هدف، نقش دو سرور، حامل‌ها، نمودار اجزا
2. تونل اصلی l3mtcp از سر تا ته
3. لینک mtcp: TLS، احراز، استتار، نشست smux، جریان‌ها و پیام‌ها، تنظیم سوکت
4. LinkManager و انتخاب لینک
5. autopilot: الگوریتم کامل با عددها
6. تشخیص لینک خراب و بازیابی: سازوکارها و خط‌های زمانی
7. حلقه‌های کنترلی، ماتریس تعامل و شکاف‌ها
8. dgtun، UDP، FEC و encap: خلاصه و مقایسه با l3mtcp
9. پیکربندی، متغیرهای محیطی، نصب و استقرار، ابزارهای عملیاتی
10. تاریخچهٔ فازها و ایده‌هایی که امتحان و رد شده‌اند
11. «از قبل وجود دارد»: فهرست جامع برای مرور پیش از هر ایدهٔ تازه
12. محدودیت‌ها و مشاهده‌ها
13. واژه‌نامه و جدول مرجع ثابت‌ها

پیوست: فهرست نقشه‌های جزئی

---

## ۱. نمای کلی

### ۱.۱ هدف پروژه

- hs2 یک تونل دوسرورهٔ تک‌باینری (Go، ایستا، `CGO_ENABLED=0`) است میان یک سرور در ایران و یک سرور در خارج، برای عبور ترافیک کاربران از مسیری که DPI آن را **برای هر اتصال جداگانه** کند می‌کند و الگوهای شناخته‌شده را می‌گیرد.
- **ایدهٔ مرکزی نسل سوم (v3):** اتصال TCP کاربر روی سرور ایران تمام می‌شود و فقط **بایت‌هایش** روی یک جریان smux، درون یکی از N لینک TLS 1.3 واقعی (اثرانگشت Chrome)، به خارج می‌رود؛ خارج خودش اتصال تازه‌ای به پنل باز می‌کند. پس برای پورت‌های کاربر **هیچ TCP درون TCP نیست** (`engine/stream.go:18-31`). N لینک تا N برابر سقف هر اتصال را حمل می‌کنند (`engine/linkmanager.go:18-35`).
- اندازهٔ استخر لینک‌ها را یک کنترل‌گر خالص (autopilot) از روی بار واقعی تعیین می‌کند (`engine/autopilot.go:11-21`)، و مجموعه‌ای از قواعد سلامت، لینک خراب را کنار می‌گذارد و جایگزین می‌کند.
- خانوادهٔ دوم، **dgtun**، برای مسیرهای پراتلاف ساخته شده: یک TUN روی استخری از حامل‌های datagram (Noise + FEC تطبیقی + کنترل نرخ تأخیرمحور) روی udp/icmp/gre/ipip/ipx (`engine/dgpool.go:18-44`). l3mtcp از هیچ‌کدام از این‌ها استفاده نمی‌کند.
- **تونل اصلی کاربر `l3mtcp` است:** همان هستهٔ جریانی mtcp به‌اضافهٔ یک رابط `hs0` (TUN) که فقط **کانال جانبی** برای پینگ و ترافیک سبک است (`README.md:659-666`؛ `install.sh:1442-1443`).

### ۱.۲ نقش دو سرور

| نقش | کلید پیکربندی | کار | لایهٔ smux |
|---|---|---|---|
| **لبه (edge)** = سرور ایران | `"mode": "dial"` | پورت‌های کاربر (`forward_ports`) را باز می‌کند؛ `LinkManager` و autopilot و همهٔ قواعد سلامت **فقط اینجا** اجرا می‌شوند؛ همهٔ جریان‌ها (کاربر، `kindL3`، کنترل، آمار، info، کنترل استخر) را باز می‌کند | همیشه client |
| **خروجی (exit)** = سرور خارج | `"mode": "listen"` | جریان‌ها را می‌پذیرد و به پنل (`expose`/`port_map`) وصل می‌کند؛ هیچ منطق سلامت لینک ندارد؛ در حالت معکوس از هدف لبه پیروی می‌کند | همیشه server |

- **واقعیت:** کلید `reverse` فقط این را عوض می‌کند که **چه کسی TCP/TLS را شماره می‌گیرد**؛ نقش‌ها ثابت‌اند: `dialing = (Mode=="dial") != Reverse` (`cmd/hs2/main.go:430, 907`؛ `engine/stream_reverse.go:14-24`). در direct، ایران کلاینت TLS است و خارج سرور TLS با گواهی و سایت پوششی؛ در reverse برعکس، و گواهی و سایت پوششی روی ایران است.
- **واقعیت:** `run` خودش `checkConfig` را اجرا نمی‌کند؛ `mode` غیر از `dial` بی‌صدا «خروجی» تعبیر می‌شود (`cmd/hs2/main.go:468`) و `unhex` خطای هگز کلید را نادیده می‌گیرد (`cmd/hs2/main.go:1005`). نصب‌کننده پیش از نصب `hs2 check` را اجرا می‌کند.

چهار شکل اجرای l3mtcp (همه با `withTUN=true`):

| شکل | پیکربندی | شماره‌گیر TLS | سازنده |
|---|---|---|---|
| لبهٔ مستقیم | `mode=dial`، `reverse=false` | ایران (`NewMTCPDialer`) | `cmd/hs2/main.go:470-502`؛ `engine/stream_iran.go:56-176` |
| خروجی مستقیم | `mode=listen`، `reverse=false` | — (گوش می‌دهد؛ `MaxLinks` فقط گزارشی) | `cmd/hs2/main.go:506-547`؛ `engine/stream_kharej.go:81-157` |
| لبهٔ معکوس | `mode=dial`، `reverse=true` | — (گوش می‌دهد؛ گواهی لازم) | `cmd/hs2/main.go:489-498`؛ `engine/stream_reverse.go:47-101` |
| خروجی معکوس | `mode=listen`، `reverse=true` | خارج (`exitPool`، شروع با `WarmSize`=۸) | `cmd/hs2/main.go:520-540`؛ `engine/stream_reverse.go:109-192`؛ `engine/exit_pool.go` |

### ۱.۳ حامل‌ها و کاربرد هرکدام

`switch fc.Carrier` در `cmd/hs2/main.go:405-424`:

| `carrier` | تابع | تعداد لینک | TUN | سیم | رمزنگاری | در نصب‌کننده | کاربرد |
|---|---|---|---|---|---|---|---|
| `mtcp` | `runStream(false,0)` | استخر خودتنظیم `min..max` | ندارد | TCP + TLS 1.3 + smux | TLS + احراز EKM | tcp → TLS mode 1 (پیش‌فرض) | پورت‌های کاربر روی چند لینک |
| **`l3mtcp`** / `l3` | `runStream(true,0)` (`:410-411`) | استخر خودتنظیم | `hs0` کانال جانبی | همان + جریان `kindL3` روی هر لینک | همان | tcp → TLS mode 2، یا tun → 6) tcp → 1) mtcp+tun | **تونل اصلی کاربر** |
| `tls` | `runStream(true,1)` | دقیقاً ۱ (`min=max=1`) | کانال جانبی | همان | همان | tcp → 3، یا tun → tcp → 2 | تک‌لینک؛ زیر throttle هر اتصال فرو می‌ریزد (۴٫۴ در برابر ۲۶ Mbit/s؛ ثبت `4ff443a`) |
| `dgtun` | `runDgTun` (`:829-903`) | استخر حامل datagram | مسیر **اصلی**، با offload | Noise + FEC روی encap | Noise IKpsk2 + ChaCha20-Poly1305 | tun → udp/icmp/gre/ipip/ipx | تونل مسیریابی‌شدهٔ حجیم روی مسیر پراتلاف |
| `udp` | `runUDP` (`:795-821`) | ۱ حامل (موتور تک‌حاملی) | مسیر اصلی | Noise + FEC روی UDP | Noise | transport 2 | تونل IP بی‌پورت کاربر |
| `auto` | `runUDP` | ۱ حامل | مسیر اصلی | کاوش UDP، در غیر این صورت **noise روی TCP** (نه TLS) | Noise | transport 1 (پیش‌فرض منو!) | تونل IP بی‌پورت کاربر |
| `noise` / `""` | `runNoise` (`:925-935`) | ۱ | مسیر اصلی | Noise روی TCP | Noise | نیست | قدیمی |
| `reality` | `runReality` (`:432-444`) | ۱ | مسیر اصلی | TLS با امضای session-id | TLS | نیست | آزمایشی؛ گواهی داغ بارگذاری نمی‌شود |

- **واقعیت (آزمایشگاه، ثبت `4ff443a`، RTT حدود ۷۶ms، Mbit/s):** مسیر تمیز ۱۰۰Mbit: mtcp ۸۷٫۳، l3mtcp ۸۷٫۷، tls ۸۷٫۰؛ ۱٪ اتلاف: ۸۶٫۹ / ۸۹٫۳ / ۸۴٫۲؛ ۵Mbit برای هر اتصال: ۲۶٫۵ / ۲۶٫۲ / **۴٫۴**. نتیجهٔ ثبت‌شده: «l3mtcp در عمل همان موتور mtcp است».
- **واقعیت:** `auto` پیش‌فرض منوی Transport است (`install.sh:1390, 1396`) ولی تونل IP بی‌پورت کاربر و بی‌`expose` می‌سازد؛ README (`:756-758`) بازگشتش را «tcp/TLS» می‌گوید در حالی که کد به حامل `noise` روی TCP برمی‌گردد (`engine/carrier_udp.go:86-90`).

### ۱.۴ نمودار اجزا (l3mtcp، لبهٔ مستقیم)

```
                 ┌──────────────────────── سرور ایران — لبه (mode=dial) ────────────────────────┐
 کاربر ─TCP/UDP─►│ پورت‌های کاربر (ListenReuse، بدون MPTCP) ─► serveUserTCP / serveUserUDP       │
                 │        └─► openStream ─► pickWait ─► LinkManager.Pick (pickKey، لایه‌ها)      │
 برنامه/هسته ───►│ hs0 (TUN، بدون offload) ─► l3Set.pumpTun ─► rendezvous ─► صف ۲۵۶/۶۰ms هر لینک│
                 │                                                                              │
                 │ LinkManager.Run — هر ۲s: reap → sampleHealth → heal → autopilot+reconcile     │
                 │                           → drainTick → publishStats                         │
                 │   ├ autopilot.decide → T (تعداد serving)                                     │
                 │   ├ queueDial → dialGate (≤۸ هم‌زمان، فاصلهٔ ۴۰–۱۶۰ms) → tlscarrier.DialFrom │
                 │   └ OnLink(هر لینک): openControl، openStats، openInfo، [openPoolCtl]، openL3  │
                 │ wedge guard (یک goroutine، هر ۲s) │ status writer (هر ۲s) → SetTCPMemPressure │
                 └───────────────┬──────────────────────────────────────────────────────────────┘
                                 │  N لینک موازی؛ هر لینک (از پایین به بالا):
                                 │  TCP: BBR، NOTSENT_LOWAT 32KiB، USER_TIMEOUT 20s، NODELAY
                                 │   └ TLS 1.3: uTLS HelloChrome_133 ⇄ crypto/tls + احراز وابسته به EKM
                                 │      └ shapedConn (رکورد ۴۰..۱۴۰۰ بایت) → meteredConn → watchConn
                                 │         └ smux v2 (قاب 16KiB، پنجره 2MiB، سطل 8MiB، keepalive ۴–۸s)
                 ┌───────────────▼──────────────────────── سرور خارج — خروجی (mode=listen) ─────────┐
                 │ tlscarrier.Server.Handle: کاوشگر ← سایت پوششی؛ کلاینت درست ← onTunnel           │
                 │ smux server → serveStream (بایت نوع، مهلت ۱۰s):                                  │
                 │   kindTCP/kindTCPPort → RouteTable.Target → DialTimeout 5s → پنل                 │
                 │   kindUDP/kindUDPPort → net.Dial udp → relayUDPConn                              │
                 │   kindL3 → l3Set (pumpTun خودش) → dev.Write → hs0 → هسته (NAT فقط دستی)         │
                 │   kindCtrl/kindStats/kindInfo → پاسخ؛ kindPool → exitPool (فقط reverse)          │
                 │ wedge guard │ status writer (پرچم فشار حافظه را در رکورد kindStats می‌فرستد)    │
                 └──────────────────────────────────────────────────────────────────────────────────┘
```

### ۱.۵ بسته‌ها و جایگاهشان

| بسته | روی مسیر l3mtcp؟ | نقش |
|---|---|---|
| `cmd/hs2` | بله (سیم‌کشی) | زیرفرمان‌ها (`run`، `check`، `doctor`، `status`، `tune`، `config`، `ports`، `recommend-links`، `cleanup`، `keygen`، `version`)، نویسندهٔ وضعیت، گواهی داغ، سایت پوششی |
| `engine` | بله | هستهٔ جریانی (`stream*.go`، `mtcp_link.go`، `linkmanager.go`، `autopilot.go`، `health.go`، `stuck.go`، `loss.go`، `refill.go`، `dialgate.go`، `wedge.go`، `control.go`، `stats.go`، `peerinfo.go`، `exit_pool.go`، `l3_link.go`، `shape.go`، `routes.go`)؛ همچنین dgtun (`dgpool.go`، `dgfq.go`، `dgports.go`، `dgforward.go`، `reorder.go`، `tunbatch.go`) و موتور تک‌حاملی (`engine.go`) |
| `tlscarrier` | بله | شماره‌گیری/پذیرش TLS، احراز، تنظیم سوکت |
| `obfs` | بله (فقط `LengthSampler`) | توزیع طول رکورد |
| `tun` | بله (`tun_linux.go` بدون offload) | دستگاه TUN |
| `tune` | بله | sysctl و سقف خودکار لینک |
| `core` | جزئی (فقط `TypeData`/`TypePing` و `KeepalivePad`) | Noise و قاب datagram برای حامل‌های datagram |
| `udpcarrier`، `fec`، `mmsg`، `encap` | **خیر** | حامل datagram، FEC، دسته‌ای کردن syscall، کپسول‌ها |
| `reality` | خیر | آزمایشی |
| `lab` | ابزار | آزمایشگاه |

- **واقعیت:** آخرین تغییر **منطق** مسیر جریانی در فاز Q8 (ثبت `f9b668c`) است؛ کار سه هفتهٔ آخر (فازهای V، W، X، Y، CA) فقط روی dgtun بوده است (نقشهٔ ۱۲؛ `git diff f9b668c HEAD -- engine/linkmanager.go` فقط فیلدهای نمایشی `PoolStats` را عوض کرده).

---

## ۲. تونل اصلی l3mtcp از سر تا ته

### ۲.۱ راه‌اندازی و سیم‌کشی

ترتیب `runCmd` (`cmd/hs2/main.go:322-425`):

1. `defer encap.ReleaseAllEchoGuards()` (`:325`) — تنها تماس l3mtcp با `encap`؛ بی‌اثر.
2. `applyTuning()`: متغیرهای آزمایشگاهی `HS2_TUNE_*` (`:258-275`).
3. `setMemoryLimit()`: حد نرم heap برابر نصف RAM (آگاه از cgroup) مگر `GOMEMLIMIT` (`:309-320`).
4. خواندن JSON؛ `bind_local_ip` نامعتبر ⇒ خروج کشنده (`:332-344`).
5. `buildTunePlan` و اگر root و `HS2_NO_TUNE` خالی ⇒ `plan.Apply` (sysctl)؛ اگر `HS2_TUNE_CC` نباشد، cc طرح روی `tlscarrier.CongestionControl` می‌نشیند (`:351-359`).
6. لاگ سقف استخر (`ceilingLogLine`، `:360-362`).
7. `runStream` (`:453-548`): `tun.Open(fc.Iface, fc.LocalCIDR, fc.PeerIP, mtu)` با `mtu = fc.MTU` یا **۱۳۸۰** (`:458-466`). `tun.Open` همان `OpenWith(..., Options{})` یعنی **بی‌offload** است (`tun/tun_linux.go:67-69`)؛ پیش از ساخت رابط هم‌نام را با `ip link del` پاک می‌کند (`:100-102`) و سپس `ip addr replace`، `mtu`، `txqueuelen 2000`، `up` و `ip route replace <peer>/32` را اجرا می‌کند (`:122-130`). پرچم‌ها `IFF_TUN|IFF_NO_PI` (`:80`). TUN **یک بار برای کل عمر فرایند** باز می‌شود (`tun/tun_linux.go:1-5`). sysctlها **پیش از** باز شدن hs0 اعمال شده‌اند، پس `default_qdisc` (پیش‌فرض `fq_codel`) احتمالاً روی hs0 می‌نشیند (**نامطمئن**؛ به درایور tun بستگی دارد).

لبه (`RunIran`، `engine/stream_iran.go:56-176`):
- ساخت `LinkManager` (مستقیم با `cfg.Dialer`؛ معکوس با `nil` و `accept=true`، `:62-73`)، `SetDrainIdle` (فقط اگر ≠۰)، `SetWarm`، `gateInfo=true` (`:74-80`).
- `l3Set` با `pumpTun` و `logDrops` (`:90-95`).
- `OnLink` برای هر لینک تازه (در goroutine خودش؛ `engine/linkmanager.go:434-435, 946-947`) این‌ها را راه می‌اندازد (`engine/stream_iran.go:101-128`):
  1. `go openControl` (کانال سلامت)؛
  2. `go openStats` (فشار سمت ارسال خروجی)؛
  3. goroutine `openInfo` (تا جواب؛ اگر جواب نیامد هر ۱ دقیقه دوباره)؛
  4. فقط در معکوس: `go openPoolCtl`؛
  5. اگر TUN هست: `openL3` **هم‌گام** درون همان goroutine.
- `go lm.Run(ctx)` یا در معکوس `runAccept` + `acceptReverseLinks` (`:133-136`).
- پورت‌های کاربر: `ListenReuse` (TCP ساده، `SO_REUSEADDR`، MPTCP خاموش) + حلقهٔ accept با `acceptBackoff` (۵ms تا ۱s)؛ با `"udp": true` یک `ListenPacket` هم. خطای bind پورت کاربر ⇒ بازگشت از `RunIran` ⇒ `must` ⇒ خروج و ری‌استارت systemd پس از ۳s (`:138-173`).
- `OnStart(statsFn)` ⇒ `startStatusWriter` (`:130-132`).

خروجی مستقیم (`RunKharej`، `engine/stream_kharej.go:81-157`): پذیرش با `acceptBackoff`، `Server.Handle` در goroutine هر اتصال، `newSession(server)` با `linkMeter` (بدون `statsPoll`) و **نمونه‌گیر طول تازه برای هر نشست**، ثبت در `peers`، لاگ `link up from %s (now %d)`، حلقهٔ `AcceptStream` و `go serveStream`. خروجی معکوس: `runKharejReverse` با `exitPool` (بخش ۳.۸).

### ۲.۲ دو مسیر جدا روی همان لینک‌ها

| ترافیک | ورود | حمل | خروج |
|---|---|---|---|
| TCP کاربر روی `forward_ports` | `ListenReuse` پورت کاربر (`engine/stream_iran.go:138-161`) | **یک** جریان smux روی **یک** لینک تا پایان عمر اتصال، سرآیند `[kindTCP]` یا `[kindTCPPort][port u16]` | خروجی `net.DialTimeout("tcp", target, 5s)` به پنل/`port_map` (`engine/stream_kharej.go:199-244`) |
| UDP کاربر (اگر `"udp": true`) | `ListenPacket` | یک جریان smux برای هر نشانی کلاینت؛ دیتاگرام `[len u16][payload]` | `net.Dial("udp", target)` |
| هر بستهٔ IP که هسته به hs0 بدهد | `pumpTun` | قاب `TypeData` روی جریان `kindL3` لینکِ برگزیدهٔ rendezvous | `dev.Write` روی hs0 سمت دیگر |
| کنترل، آمار، info، کنترل استخر | داخلی | جریان‌های خام `kindCtrl`/`kindStats`/`kindInfo`/`kindPool` | داخلی |

**چه چیزی اصلاً به hs0 می‌رسد (واقعیت):**
- hs2 روی hs0 فقط زیرشبکهٔ متصل `local_cidr` (یک ‎/30 درون `10.77.0.0/16`؛ ایران base+1، خارج base+2؛ `install.sh:37-47, 604-609`) و مسیر `peer_ip/32` را می‌گذارد (`tun/tun_linux.go:122-130`).
- در کل کد Go و `install.sh` هیچ `ip_forward`، `MASQUERADE`، `TCPMSS`/MSS clamp یا `ip rule` نیست (grep؛ نقشه‌های ۰۶، ۲۰، ۹۱). قاعدهٔ iptables/nft فقط برای پاسخ ICMP در encap است و آن را **باینری** می‌گذارد (`encap/echoguard_linux.go`)، نه نصب‌کننده (نصب‌کننده فقط بسته‌های `iptables`/`nftables` را نصب می‌کند، `install.sh:282-287`).
- پس بدون کار دستی اپراتور، ترافیک hs0 فقط بین دو نشانی تونل است (پینگ و ترافیک سبک). نصب‌کننده در حالت «بی‌پورت کاربر» هشدار می‌دهد که کاربر فقط وقتی به پنل می‌رسد که اپراتور خودش ترافیک را به‌سوی IP تونل خارج مسیر دهد (`install.sh:2144, 2289`). **هر ترافیک حجیمی که از hs0 بگذرد TCP درون TCP است.** اینکه کاربر این پروژه hs0 را برای ترافیک حجیم به کار می‌برد یا فقط برای پینگ، از کد معلوم نیست (**نامطمئن**).
- آزمون `TestTunModeReverseWithUserPorts` هم‌زیستی دو مسیر روی همان لینک‌ها را تضمین می‌کند (`engine/tun_mode_test.go:199-237`).

### ۲.۳ مسیر رفت یک بستهٔ hs0 (ایران ← خارج)

| # | گام | محل | صف/بافر | مسدود شدن / دورریز |
|---|---|---|---|---|
| 1 | هسته بسته را به hs0 مسیر می‌دهد | `tun/tun_linux.go:122-130` | qdisc رابط + `txqueuelen 2000` | دورریز هسته در hs2 دیده نمی‌شود (**نامطمئن**) |
| 2 | `pumpTun` → `dev.Read` (یک syscall مسدودکننده برای هر بسته؛ fd بدون `O_NONBLOCK`) | `engine/l3_link.go:334-350`؛ `tun/tun_linux.go:74, 117, 155-158` | بافر `MTU+128` از `sync.Pool` | خطای خواندن ⇒ `continue` **بدون مکث** |
| 3 | `flowHash`: FNV-1a روی (src، dst، proto، sport، dport) برای IPv4 TCP/UDP؛ سه‌تایی برای پروتکل‌های دیگر؛ فقط نشانی‌ها برای IPv6؛ offset قطعه بررسی نمی‌شود | `engine/l3_link.go:399-415` | — | — |
| 4 | `l3Set.pick`: rendezvous، بیشینهٔ `mix32(h ^ l.id)` روی لینک‌های `Alive()` (فقط پرچم `dead`؛ **بی‌خبر از سلامت لینک**) | `engine/l3_link.go:305-323` | زیر `RLock`، O(تعداد لینک) | بی‌لینک ⇒ دورریز، `drops++` |
| 5 | `enqueue` غیرمسدود | `engine/l3_link.go:192-199, 352-355` | کانال **۲۵۶** بسته (`l3QueueLen`، `:53`)؛ زمان ورود ثبت می‌شود | صف پر ⇒ دورریز؛ `pumpTun` **هرگز** مسدود نمی‌شود (`TestPumpNeverBlocksOnStuckLink`) |
| 6 | `writeLoop` (تنها نویسندهٔ L3 هر لینک) | `engine/l3_link.go:202-243` | دسته تا `l3BatchBytes=16KiB` (+یک بسته؛ ظرفیت اولیه ۲۰KiB)؛ قاب ۷ بایتی `[ftype][len24][pad24=0]` (`tlscarrier/carrier.go:34-38`) | سن > **۶۰ms** (`l3MaxSojourn`، `:62`) هنگام برداشتن ⇒ دورریز شمرده‌شده؛ همه کهنه ⇒ نوشتنی نیست |
| 7 | keepalive در بی‌کاری نوشتن | `engine/l3_link.go:131-136, 234-241` | قاب `TypePing` با ۰..۹۵ بایت صفر (`core/shape.go:100`) | ۲s، و پس از اعلام `capL3Quiet` همتا ۸ تا ۱۲s |
| 8 | `streamPkt.WriteRaw`: `SetWriteDeadline(now+5s)` و `Stream.Write` | `engine/stream.go:214-218` | — | **هر خطا، از جمله گذشتن ۵s** (انتظار پشت نویسندهٔ smux **یا** انتظار پنجرهٔ همتا) ⇒ `markDead` **دائمی** (`engine/l3_link.go:237-240`؛ `smux@/stream.go:404-420`) — **سنجیده** |
| 9 | کنترل جریان smux v2 | `smux@/stream.go:339-425` | پنجرهٔ همتا ابتدا 256KiB (`smux@/frame.go:28`) و پس از نخستین UPD همان 2MiB؛ خرد شدن به قاب‌های ≤16KiB | پنجره پر ⇒ انتظار |
| 10 | صف نوشتن smux | `smux@/session.go:425-463, 530-557`؛ `smux@/shaper.go:10-15` | heap تا ۱۰۲۴ درخواست؛ اول کلاس CTRL، سپس شمارهٔ ترتیب سراسری | **L3 هیچ اولویتی ندارد**؛ هر جریان یک قاب در راه؛ با N جریان حجیم، دستهٔ L3 پشت حداکثر N قاب ۱۶KiB |
| 11 | `sendLoop` (تنها نویسندهٔ سوکت لینک) | `smux@/session.go:465-521` | بافر 64KiB+8؛ سرآیند ۸ بایتی + داده با یک `conn.Write` | خطا ⇒ پایان حلقه |
| 12 | `watchConn` → `meteredConn` → `shapedConn` | `engine/stream.go:117-123`؛ `engine/health.go:183-195`؛ `engine/shape.go:63-103` | `wrBlocked` فقط برای Writeهای >۱ms؛ هر تکه یک رکورد TLS | نخستین خطای I/O ⇒ بستن **کل لینک** |
| 13 | TLS 1.3 و سوکت TCP | `tlscarrier/tune_linux.go:31-51` | `NOTSENT_LOWAT=32KiB`؛ بافر ارسال تا `tcp_wmem` بیشینه (۸/۱۶/۳۲ MiB) | دادهٔ تأییدنشدهٔ ۲۰s ⇒ خطای سوکت |
| 14 | خروجی: `shapedConn.Read` → `meteredConn` → `watchConn` (`rdCalls++`) → `recvLoop` | `engine/shape.go:107-132`؛ `engine/stream.go:106-115`؛ `smux@/session.go:320-397` | **سطل نشست 8MiB** مشترک همهٔ جریان‌های لینک | سطل ≤0 ⇒ `recvLoop` از سوکت نمی‌خواند و همهٔ جریان‌ها، از جمله L3، می‌ایستند |
| 15 | `linkToTun` → `ReadFrameReuse` | `engine/l3_link.go:361-377`؛ `engine/stream.go:220-238` | مهلت خواندن هر قاب **۳۰s** (`l3StreamDeadAfter`)؛ سقف قاب ۶۵۵۳۶ | خطا/مهلت/قاب بزرگ ⇒ `markDead` |
| 16 | `dev.Write(payload)` **هم‌گام و بی‌بررسی خطا** | `engine/l3_link.go:373-375`؛ `tun/tun_linux.go:227-230` | صف ورودی هسته (`netdev_max_backlog`) | — |
| 17 | هستهٔ خارج | — | — | تحویل محلی برای IP تونل؛ forward فقط اگر اپراتور ساخته باشد |

### ۲.۴ مسیر برگشت

- **واقعیت:** قرینه است با `l3Set` سمت خارج، که `pumpTun` و `logDrops` خودش را دارد (`engine/stream_kharej.go:87-92`) و برای هر جریان `kindL3` پذیرفته‌شده یک `l3Link` می‌سازد (`:245-253`). شناسهٔ rendezvous هر لینک در هر سمت جداگانه با `rand.Uint32()` ساخته می‌شود (`engine/l3_link.go:113`) و `flowHash` جهت‌دار است (`:404-408`)، پس **رفت و برگشت یک جریان معمولاً روی دو لینک متفاوت می‌افتند** (استنتاج). RTT که TCP درونی می‌بیند جمع دو مسیر یک‌طرفهٔ متفاوت است.
- **واقعیت:** سمت خروجی هیچ منطق سلامت لینک ندارد (`LinkManager` فقط در `RunIran` ساخته می‌شود)؛ L3 خروجی هم فقط `dead` را می‌بیند و فقط با مهلت ۵s نوشتن، سکوت ۱۲s نشست، مهلت ۳۰s خواندن یا بسته شدن نشست می‌میرد.

### ۲.۵ نقاط دورریز hs0 و دیده‌شدنشان

| محل | شرط | در `drops` شمرده می‌شود؟ | لاگ |
|---|---|---|---|
| qdisc/حلقهٔ hs0 هسته | پر شدن | خیر | خیر (شاید `ip -s link`؛ نامطمئن) |
| `pumpTun` | هیچ لینک L3 زنده‌ای نیست | بله | هر ۳۰s: `l3: dropped %d packets in 30s on the tun side channel (queue limit or no link) — in the TLS modes the tun is for ping and light traffic; the user ports carry the bulk, unaffected` (`engine/l3_link.go:380-393`) |
| `enqueue` | صف ۲۵۶ پر | بله | همان؛ علت‌ها از هم جدا نیستند |
| `writeLoop` | سن > ۶۰ms | بله | همان |
| `markDead` | بسته‌های ماندهٔ صف لینک مرده | **خیر** (**سنجیده**: با لینک گیرکرده `drops=0` ماند) | خیر |
| `markDead` | قاب‌های مانده در heap smux یا سوکت | خیر | خیر (شاید هنوز برسند؛ نامطمئن) |
| گیرنده | قاب جریان بسته‌شده؛ قاب بزرگ‌تر از حد (`engine: oversized L3 frame` فقط برگشت داده می‌شود) | خیر | خیر |
| `dev.Write` | خطای هسته | خیر | خیر |

- **واقعیت:** فایل وضعیت l3mtcp هیچ فیلد `tun_*` ندارد (این فیلدها فقط در `runDgTun` پر می‌شوند؛ `cmd/hs2/status.go:309-322`). مرگ یک لینک L3، شکست `openL3` و لینکی که بی‌L3 مانده **هیچ لاگی ندارند**.

### ۲.۶ مسیر یک اتصال TCP کاربر

| # | گام | محل | صف/زمان‌سنج | نکته |
|---|---|---|---|---|
| 1 | پذیرش روی `user_listen_ip:port` | `engine/stream_iran.go:141-161`؛ `engine/listen.go:12-43` | keepalive پیش‌فرض Go (۱۵s/۱۵s/۹)؛ `tcp_notsent_lowat=128KiB` سراسری | هسته دست‌دهی را پیش از هر چیز تمام کرده |
| 2 | `serveUserTCP` → `openStream` (تا **۳** تلاش) | `:159, 241-268` | — | لینک بین Pick و `OpenStream` ممکن است بمیرد |
| 3 | `pickWait(hold=true)` | `:183-210` | ۴۰ × ۱۵۰ms ≈ **۶s**؛ در اپیزود refill تا **۱۰s** (`engine/refill.go:50`) | بی‌لینک ⇒ بستن اتصال کاربر |
| 4 | `Pick` زیر قفل نوشتن | `engine/linkmanager.go:1514-1586` | لایه‌ها و `pickKey` (بخش ۴.۳) | — |
| 5 | `OpenStream`: SYN با کلاس CTRL | `engine/mtcp_link.go:59-73`؛ `smux@/session.go:122-167` | مهلت `openCloseTimeout=30s` | روی لینکی که نویسنده‌اش گیر است **تا ۳۰s** می‌ماند (استنتاج) |
| 6 | سرآیند `userStreamHeader` | `engine/stream_iran.go:219-237, 250` | — | **بدون مهلت** |
| 7 | `relayStream(user, st, guard)` | `engine/wedge.go:295-345` | دو کپی‌کننده با بافر 32KiB (`engine/stream.go:53`)؛ جهت جریان→کاربر از `watchedWriter` | نیم‌بسته پشتیبانی نمی‌شود؛ جریان کاربر مهلت نوشتن ندارد |
| 8 | لایه‌های لینک | همان گام‌های ۹ تا ۱۴ بخش ۲.۳ | پنجرهٔ جریان 2MiB (ابتدا 256KiB) | — |
| 9 | خروجی: `AcceptStream` → `serveStream` | `engine/stream_kharej.go:144-151, 173-211` | صف پذیرش ۱۰۲۴؛ نوع و پورت هرکدام مهلت ۱۰s (`kindTimeout`) | — |
| 10 | `routeTable().Target(port)` | `engine/routes.go:39-46` | — | بی‌هدف ⇒ لاگ محدود و بستن |
| 11 | `net.DialTimeout("tcp", target, 5s)` | `engine/stream_kharej.go:233-237` | بدون `LocalAddr` | شکست ⇒ بستن **بی‌لاگ**؛ کاربر فقط EOF می‌بیند |
| 12 | رلهٔ خروجی | `:238-244` | دو کپی‌کننده 32KiB؛ زیر نظر wedge guard | — |

- **پایان:** هر جهت که تمام شود، هر دو سر بسته می‌شوند (`engine/wedge.go:324-344`). با مرگ نشست لینک، کپی‌کنندهٔ جریان→کاربر بافر مانده را تحویل می‌دهد و حداکثر پس از `relayDieGrace=5s` هر دو بسته می‌شوند (`:68-70, 328-339`)؛ بستن عادی FIN است و فقط `kill` نگهبان با `SetLinger(0)` RST می‌فرستد (`:298-306`).
- **واقعیت:** اتصال روی لینک دیگری ادامه نمی‌یابد؛ مهاجرت وجود ندارد (`engine/linkmanager.go:23-27`). هر خرابی لینک برای اتصال‌های TCP آن لینک یعنی قطع و اتصال دوباره.
- **UDP کاربر** (`engine/stream_iran.go:270-416`): هر نشانی کلاینت یک `udpFlow` با صف ۲۵۶ دیتاگرام و حداکثر 512KiB و نویسندهٔ جدا؛ صف پر ⇒ دورریز؛ جریان با `pickWait(hold=false)` باز می‌شود (در refill نگه داشته نمی‌شود)؛ خطا ⇒ `gone()` و دیتاگرام بعدی جریان تازه می‌سازد؛ بی‌کاری ۲ دقیقه (جاروب هر ۳۰s). در خروجی مهلت خواندن از پنل ۲ دقیقه است و UDP زیر نظر wedge guard نیست (`engine/stream.go:270-300`).
- **نشانی واقعی کاربر به پنل نمی‌رسد:** PROXY protocol یا روش مشابهی نیست (grep صفر؛ `engine/stream_kharej.go:233`).

### ۲.۷ اشتراک لایه‌ها میان hs0 و کاربران (استنتاج از کد)

- یک `sendLoop`، یک heap FIFO، یک سطل 8MiB و یک سوکت TCP بین همهٔ جریان‌های یک لینک مشترک است. پس دانلودهای حجیم روی یک لینک تا N قاب 16KiB جلوی دستهٔ L3 صف می‌سازند؛ سقف ۶۰ms فقط زمان صف خود L3 را می‌سنجد، نه heap smux، سوکت، شبکه یا سطل گیرنده (`engine/l3_link.go:207`).
- اگر سطل 8MiB با خوانندگان کند پر شود (wedge)، L3 همان لینک هم می‌ایستد و اگر ≥۱۲s طول بکشد، `watchSession` آن را برای همیشه می‌کشد.
- جریان L3 «بار کاربر» نیست (`OpenRawStream`، `engine/mtcp_link.go:183`)؛ پس در `flowing`، کف autopilot و شرط بستن لینک بازنشسته دیده نمی‌شود، ولی بایت‌هایش در نرخ لینک، `G` و `wrBlocked` هست (بخش ۵.۹).

### ۲.۸ ترتیب بسته‌ها

- **درون یک لینک ترتیب حفظ می‌شود** (یک `pumpTun`، صف FIFO، یک `writeLoop`، جریان smux مرتب، یک خواننده؛ آزمون `TestWriteLoopDeliversAndKeepsAlive`). دورریز حذف می‌کند ولی جابه‌جا نمی‌کند.
- **نگاشت جریان به لینک** در این حالت‌ها عوض می‌شود:
  1. مرگ L3 لینکش: فقط جریان‌های همان لینک می‌روند (`TestPickMovesOnlyDeadLinksFlows`).
  2. **افزوده شدن هر لینک:** حدود 1/(N+1) جریان‌ها به لینک تازه می‌روند — **سنجیده**: ۲→۳ لینک ۳۳٫۳٪، ۴→۵ ۱۹٫۷٪، ۸→۹ ۱۰٫۸٪، ۳۲→۳۳ ۲٫۷٪. توضیح کد (`engine/l3_link.go:305-307`) فقط از مرگ می‌گوید. لینک تازه از شماره‌گیری، `AddLink` معکوس (حتی spare)، پروب رشد، `heal`، `reconcile` و refill می‌آید.
  3. degraded/suspect/draining/retiring/pressed هیچ اثری ندارد؛ ولی **بسته شدن** لینک retiring/draining، L3 آن را می‌کشد و جریان‌هایش را جابه‌جا می‌کند.
- **پیامد (استنتاج):** در «افزوده شدن» لینک قبلی زنده است و بسته‌های در راه (صف ≤۶۰ms، heap smux، ۳۲KiB ارسال‌نشده، پنجرهٔ ازدحام، سطل گیرنده) **دیر ولی کامل** پس از نخستین بسته‌های لینک تازه می‌رسند ⇒ بی‌ترتیبی واقعی.
- **دانه‌بندی هش:** همهٔ پینگ‌ها/ICMP/GRE دو میزبان در هر جهت روی **یک** لینک؛ IPv6 فقط با نشانی‌ها؛ قطعه‌های یک دیتاگرام IPv4 ممکن است از لینک‌های مختلف بروند.
- **در l3mtcp هیچ بازچین (`reorderer`)، جدول چسبندگی (`sticky`)، `tunBatch`، offload یا صف منصفانه نیست**؛ همه فقط در dgtun ساخته می‌شوند (`engine/dgpool.go:549-556, 670-671`).
- **مسیر TCP پراکسی‌شده:** جابه‌جایی ترتیب ممکن نیست (یک جریان، یک لینک). UDP: پس از مرگ جریان، دیتاگرام‌های در راه گم می‌شوند نه جابه‌جا.

### ۲.۹ جدول صف‌ها، بافرها و زمان‌سنج‌ها (از سوکت کاربر تا سوکت مقصد)

**فقط hs0:**

| نام | مقدار | محل |
|---|---|---|
| MTU hs0 | `mtu` پیکربندی یا ۱۳۸۰ (نصب‌کننده: بخش ۲.۱۱) | `cmd/hs2/main.go:458-461` |
| `txqueuelen` | 2000 | `tun/tun_linux.go:125` |
| بافر خواندن TUN | `MTU+128` | `engine/l3_link.go:335-342` |
| `l3QueueLen` | ۲۵۶ بسته برای هر لینک | `engine/l3_link.go:53` |
| `l3MaxSojourn` | 60ms (فقط زمان صف L3) | `engine/l3_link.go:62, 207` |
| `l3BatchBytes` | 16KiB (+یک بسته) | `engine/l3_link.go:56, 203` |
| مهلت `WriteRaw` | 5s | `engine/stream.go:215` |
| `l3KeepaliveEvery` / `l3QuietKeepalive` | 2s / 10s±20٪ (۸–۱۲s، با `jitterAround`) | `engine/l3_link.go:67, 79, 131-136`؛ `engine/exit_pool.go:565-567` |
| `l3StreamDeadAfter` | 30s برای هر قاب | `engine/l3_link.go:78` |
| `l3SessionSilent` (var) | 12s، تیک `min(2s, 12s/4)` ⇒ عملاً ۱۲–۱۴s | `engine/l3_link.go:87, 143-161` |
| `l3DeadAfter` | 8s — **در تولید استفاده نمی‌شود** | `engine/l3_link.go:68` |
| سقف قاب L3 | ۶۵۵۳۶ داده و ۶۵۵۳۶ پد | `engine/stream.go:227-229` |
| `logDrops` | ۳۰s | `engine/l3_link.go:380-393` |

**فقط کاربر:**

| نام | مقدار | محل |
|---|---|---|
| keepalive سوکت کاربر و پنل | پیش‌فرض Go ۱۵s/۱۵s/۹ | `/usr/local/go/src/net/dial.go:17-26` |
| `pickWait` | ۴۰×۱۵۰ms ≈ ۶s | `engine/stream_iran.go:184, 206` |
| refill | `refillHoldMax=10s`، `refillStall=3s`، `refillTick=100ms`، `refillRearm=1m`، `refillKeep=5m` | `engine/refill.go:50-58` |
| تلاش `openStream` | ۳ | `engine/stream_iran.go:243` |
| `openCloseTimeout` smux | 30s (SYN/FIN) | `smux@/session.go:17` |
| بافر رله | ۲×32KiB برای هر سمت | `engine/stream.go:53` |
| `relayDieGrace` | 5s | `engine/wedge.go:68-70` |
| خروجی: نوع/پورت | ۱۰s + ۱۰s | `engine/stream.go:51` |
| خروجی: اتصال پنل | 5s | `engine/stream_kharej.go:233` |
| UDP | صف ۲۵۶ یا 512KiB؛ بی‌کاری ۲m؛ جاروب ۳۰s | `engine/stream_iran.go:286-289, 307-325`؛ `engine/stream.go:270` |
| لینک بازنشسته | `drainIdleDefault=310s`؛ `retireForce=20m` | `engine/linkmanager.go:82, 91` |
| لینک degraded | `maxDrain=45s`، `drainStall=15s`، `maxDrainActive=90s` | `engine/health.go:74-76` |

**مشترک هر لینک:**

| نام | مقدار | محل |
|---|---|---|
| پنجرهٔ جریان smux | 256KiB سپس 2MiB؛ UPD پس از 1MiB مصرف | `smux@/frame.go:28`؛ `engine/mtcp_link.go:262`؛ `smux@/stream.go:146` |
| سطل نشست | 8MiB | `engine/mtcp_link.go:264` |
| قاب smux | ≤16KiB + سرآیند ۸ بایت | `engine/mtcp_link.go:258` |
| heap نوشتن | ≤۱۰۲۴ درخواست | `smux@/session.go:16` |
| keepalive smux | NOP هر ۴–۸s (تصادفی برای هر نشست)؛ بستن اگر در یک دورهٔ ۲۴s هیچ سرآیندی نیاید و سطل > 0 ⇒ ۲۴ تا ۴۸s | `engine/mtcp_link.go:278-279`؛ `smux@/session.go:399-422` |
| قاب شکل‌دهی | سرآیند ۴ بایت؛ ۹ اندازهٔ ۴۰..۱۴۰۰؛ سقف خواندن 32KiB | `engine/shape.go:46-51`؛ `obfs/shaper.go:41-42` |
| `TCP_NOTSENT_LOWAT` / `TCP_USER_TIMEOUT` | 32KiB / 20000ms | `tlscarrier/tune_linux.go:19, 22` |
| `tcp_rmem` / `tcp_wmem` | `4096 131072 max` / `4096 65536 max`؛ بیشینه ۸/۱۶/۳۲MiB | `tune/tune.go:280-289, 346-347` |
| مهلت اتصال TCP لینک | 8s (scout خروجی معکوس 2s) | `tlscarrier/carrier.go:162`؛ `cmd/hs2/main.go:534-536` |
| احراز/دست‌دهی | `authTimeout=10s`؛ سرور: `handshakeTimeout=10s`، `firstReadTimeout=30s` | `tlscarrier/auth.go:59`؛ `tlscarrier/server.go:40, 47` |

### ۲.۱۰ هزینهٔ هر بسته و سربار سیم (محاسبه)

- **فرستنده:** یک `read()` برای هر بسته. یک دستهٔ 16KiB حدود ۱۲ بستهٔ ۱۳۴۰ بایتی است و ۱ یا ۲ قاب smux می‌شود؛ میانگین وزنی اندازهٔ هدف شکل‌دهی ۹۹۱٫۴ بایت است، پس هر 16KiB حدود ۱۷ رکورد TLS، یعنی حدود ۱٫۴ فراخوان ارسال برای هر بسته. **گیرنده:** یک `write()` برای هر بسته روی TUN.
- **سربار:** ۷ بایت قاب L3 + ۸ بایت smux برای هر قاب + ۴ بایت شکل‌دهی و حدود ۲۲ بایت TLS برای هر رکورد. پینگ ۸۴ بایتی + ۷ + ۸ = ۹۹ بایت داده که تا اندازهٔ نمونه (میانگین ≈۹۹۱، ۵۵٪ احتمال ۱۴۰۰) پد می‌شود ⇒ حدود ده برابر روی سیم. keepaliveها، UPD، ping کنترل و کلید SSH هم همین‌طور.
- **حافظه (محاسبه):** بدترین حالت هر لینک `256 × (MTU+128)` بایت صف (حدود ۳۷۰KB با MTU 1320؛ ≈۱۱۱MB در ۳۰۰ لینک) + بافر دسته + `rbuf`.
- **goroutine:** برای هر لینک در هر سمت ۳ goroutine L3 (`writeLoop`، `linkToTun`، `watchSession`) + ۴ goroutine smux؛ در ۳۰۰ لینک ⇒ ۹۰۰ goroutine L3.

### ۲.۱۱ MTU (اصلاح‌شده)

- **باینری:** اگر `mtu` صفر/غایب باشد، ۱۳۸۰ در l3mtcp/tls (`cmd/hs2/main.go:458-461`) و ۱۲۸۰ در udp/auto/dgtun (`:367-369`، `:831-834`).
- **نصب‌کننده دو راه برای ساخت l3mtcp دارد و MTU پیش‌فرضشان فرق دارد** (اصلاح نقشه‌های ۰۶ و ۲۰؛ نقشه‌های ۰۱ و ۱۱ درست بودند):
  - راه «tcp ← TLS mode ← 2) l3mtcp»: مقدار **ثابت** `"mtu": 1380` (`install.sh:1875, 1980, 2103, 2239`)؛ نام رابط پرسیده نمی‌شود؛ درگاه کاربر اجباری است؛ در لینک `TRANSPORT=tcp` و فیلد MTU خالی.
  - راه «tun ← 6) tcp ← 1) mtcp + tun»: `ask_tun_mtu` با پیش‌فرض **۱۳۲۰** («matches Backhaul»، بازهٔ پذیرش ۶۸..۶۵۵۳۵؛ `install.sh:1664-1672`) و نوشتن `$TUNMTU` (`:1901, 2276`)؛ طرف چسباننده MTU را از لینک می‌گیرد (`:2002, 2132`)؛ درگاه کاربر اختیاری است.
- `hs2 check` فقط بیرون از ۵۷۶..۹۰۰۰ هشدار می‌دهد (`cmd/hs2/check.go:344-345`). MTU بیرونی مسیر روی hs0 اثری ندارد، چون بسته‌ها روی جریان بایتی می‌روند؛ MSS اتصال محلی روی hs0 برابر MTU−40 است و برای ترافیک عبوری فقط PMTUD هسته می‌ماند (هیچ clamp نیست). ناهمخوانی MTU دو سمت را `hs2 check` نمی‌بیند.

---

## ۳. لینک mtcp: TLS، احراز، استتار، نشست smux، جریان‌ها و پیام‌ها

### ۳.۱ پشتهٔ لایه‌های یک لینک

ساخت در `newSession` (`engine/stream.go:172-203`)، یکسان در هر دو سمت (پس شکل‌دهی متقارن است):

```
TCP     BBR (یا cc طرح tune)، TCP_NOTSENT_LOWAT=32KiB، TCP_USER_TIMEOUT=20s، NODELAY (هر دو سمت)
 └ TLS 1.3   شماره‌گیر: uTLS HelloChrome_133؛ پذیرنده: crypto/tls با گواهی واقعی؛ احراز دوطرفهٔ وابسته به EKM
    └ shapedConn    هر Write ⇒ چند قاب [dataLen u16][padLen u16][data][pad]، هر قاب = یک رکورد TLS
       └ meteredConn   rdBytes/wrBytes و wrBlocked (Write بیش از ۱ms)؛ بالای شکل‌دهی ⇒ بار واقعی
          └ watchConn     نخستین خطای I/O ⇒ بستن conn و نشست؛ rdCalls/inRead برای نگهبان wedge و ناظر L3
             └ smux v2      client روی لبه، server روی خروجی
                ├ جریان‌های کاربر: kindTCP / kindTCPPort / kindUDP / kindUDPPort
                ├ kindCtrl (ping/pong سلامت)    ├ kindStats (آمار ارسال خروجی)
                ├ kindInfo (یک‌بار؛ سقف/قابلیت/پورت‌ها)  ├ kindPool (فقط reverse)
                └ kindL3 (فقط با TUN: بسته‌های hs0)
```

- **واقعیت:** l3mtcp، mtcp و tls از رمزنگاری `core` استفاده نمی‌کنند؛ امنیت فقط از TLS 1.3 و احراز وابسته به کانال است. لایهٔ AEAD دوم عمداً اضافه نشده (`hs2-src/BUILD.md:179-180`).

### ۳.۲ برقراری لینک (direct؛ ایران شماره می‌گیرد)

1. **دروازهٔ سراسری شماره‌گیری** (`engine/dialgate.go:9-41`؛ `engine/linkmanager.go:394-396`): حداکثر ۸ دست‌دهی هم‌زمان در کل فرایند، شروع‌ها با فاصلهٔ تصادفی ۴۰ تا ۱۶۰ms (حدود ۱۰ در ثانیه). دلیل ثبت‌شده: انبوه ClientHelloهای یکسان از یک IP به یک IP:port «کاری است که هیچ مرورگری نمی‌کند»؛ و ارزیابی DPI نشان داد پراکندگی ۱۲۰–۴۸۰ms خودش نشانه است (ثبت `2634c34`).
2. `mtcpDialer.DialLink` ⇒ `tlscarrier.DialFrom(addr, sni, key, bindIP)` (`engine/mtcp_link.go:286-292`). **`ctx` به شماره‌گیری نمی‌رسد.**
3. اتصال TCP با مهلت **۸s** (`tlscarrier/carrier.go:161-163`)؛ `bind_local_ip` نامعتبر خطا می‌دهد و هرگز بی‌صدا به IP پیش‌فرض برنمی‌گردد (`:170-178`).
4. `SetKeepAlive(true)` و `SetKeepAlivePeriod(3s)` و `tuneTCP` (`:183-190`). **اصلاح:** در Go 1.27 `SetKeepAlivePeriod` فقط `TCP_KEEPIDLE` را عوض می‌کند؛ فاصلهٔ کاوش ۱۵s و تعداد ۹ پیش‌فرض Go می‌ماند (`/usr/local/go/src/net/tcpsock.go:249-257`؛ `dial.go:17-26`). توضیح کد آن را «تشخیص سیاه‌چاله در هسته» می‌نامد، ولی چون NOP smux هر ۴–۸s داده در راه نگه می‌دارد، در عمل `TCP_USER_TIMEOUT` تعیین‌کننده است و اثر keepalive **نامطمئن/حاشیه‌ای** است. سمت پذیرنده (شنوندهٔ لینک و پورت‌های کاربر) فقط پیش‌فرض Go را دارد (۱۵/۱۵/۹؛ `engine/listen.go:28-43`).
5. `utls.UClient(raw, {ServerName: sni, InsecureSkipVerify: true}, HelloChrome_133)`؛ مهلت کل دست‌دهی + احراز + اثبات **۱۰s** (`authTimeout`، `tlscarrier/auth.go:59`؛ `carrier.go:191-197`). اثرانگشت شامل X25519MLKEM768، GREASE، GREASE ECH، ALPS، فشرده‌سازی گواهی brotli و ترتیب به‌هم‌ریختهٔ پسوندهای Chrome است (utls v1.8.0).
6. الزام TLS 1.3؛ پس از تأیید، `Renegotiation=RenegotiateNever` (پیش‌تنظیم Chrome renegotiation اعلام می‌کند و utls در آن حالت EKM نمی‌دهد) و `ExportKeyingMaterial("EXPORTER-hs2-channel-binding-v2", nil, 32)` (`carrier.go:223-231`؛ `auth.go:87-89`).
7. کلاینت **یک رکورد** احراز می‌فرستد و تا اثبات سرور را نبیند **هیچ دادهٔ تونلی نمی‌فرستد** (`carrier.go:206-214`؛ `auth.go:187-203`).
8. `newEdgeLink` (`engine/mtcp_link.go:299-307`): `linkMeter` با `statsPoll` و `newSession(car.RawConn(), false, sampler, mtr)`؛ از این لحظه قاب‌بندی خود حامل TLS استفاده نمی‌شود (`tlscarrier/carrier.go:119-122`). یک `sampler` مشترک برای همهٔ لینک‌های لبه.
9. لینک با شناسهٔ یکتای `linkSeq` به استخر افزوده و `OnLink` در goroutine صدا زده می‌شود (`engine/linkmanager.go:936-948`).

**قالب احراز (v2؛ `tlscarrier/auth.go:18-51`):**
```
کلاینت → سرور (یک رکورد TLS؛ روی سیم ۳۳۶–۷۷۶ بایت):
  [nonce:16][tag:16][padlen:u16 BE][pad: تصادفی 280..720]
  tag = HMAC-BLAKE2s-256(key, "hs2-auth-v2" ‖ nonce ‖ be64(unix/60) ‖ EKM)[:16]
سرور → کلاینت (فقط پس از وارسی؛ روی سیم ۱۶۰–۵۲۰ بایت):
  [tag:16][padlen:u16 BE][pad: تصادفی 120..480]
  tag = HMAC-BLAKE2s-256(key, "hs2-srv-v2" ‖ nonce ‖ EKM)[:16]
v1 (رد می‌شود و فقط لاگ نادر): [nonce:16][HMAC(key,"hs2-tls-auth"‖nonce‖be64(minute))[:16]] = ۳۲ بایت
```
- رواداری ساعت: ±۲ سطل دقیقه‌ای (مؤثر بین ۲ و ۳ دقیقه؛ `auth.go:58, 91`). حافظهٔ nonce: TTL ۵ دقیقه، سقف ۱۶۳۸۴، FIFO با هزینهٔ سرشکن O(1) (`tlscarrier/replaymem.go:9-59`). بازپخش رکورد احراز روی اتصال دیگر به‌خاطر وابستگی به EKM کار نمی‌کند (`TestReplayRejected`).

**ماشین حالت `Server.Handle`** (`tlscarrier/server.go:72-148`):
```
پذیرش → tuneTCP → دست‌دهی TLS (مهلت 10s؛ MinVersion 1.2؛ ALPN فقط http/1.1)
  ├ RecordHeaderError و ۵ بایت اول یکی از "GET /","HEAD ","POST ","PUT /","OPTIO" → پاسخ دقیق httpToHTTPS400 گو → بستن
  ├ هر خطای دیگر → raw.Close (FIN یا RST دقیقاً مثل Go؛ آزمون تفاضلی TestProbeCloseMatchesStdlib)
  └ موفق → صدور EKM (خطا، مثلاً TLS1.2 بدون EMS → raw.Close بی‌پاسخ)
        └ خواندن «یک رکورد» در بافر 16KiB با مهلت 30s (firstReadTimeout؛ n==0 → بستن)
              ├ احراز نامعتبر → (اگر v1: logRare هر 30s) → forward به پشتیبان (سایت پوششی)
              └ معتبر → خواندن باقی پد (10s) → nonce تکراری؟ بستن : نوشتن اثبات → onTunnel(Carrier)
```
- پشتیبان پوششی: `backend_addr` یا سرور داخلی روی `127.0.0.1:0` با صفحهٔ قطعیِ ساخته‌شده از `cover_seed` (هر نصب متفاوت؛ بی‌JS؛ ETag نمک‌زده؛ `Last-Modified` نسبی ۱۸–۴۰۰ روز؛ ۴۰۴ برای هر مسیر جز `/`؛ بدون سرآیند `Server`؛ `ReadHeaderTimeout=10s`) (`cmd/hs2/main.go:781-789, 945-993`؛ `cmd/hs2/cover.go:250-450`).
- **گواهی:** کلاینت آن را وارسی نمی‌کند (`InsecureSkipVerify`؛ اعتماد فقط از EKM). سمت سرور با `certReloader` (اشاره‌گر اتمی) بارگذاری داغ با SIGHUP و پایش mtime هر دقیقه؛ بارگذاری بد گواهی قبلی را نگه می‌دارد؛ هشدار انقضا در ≤۷ روز (`cmd/hs2/cert.go:19-156`). پیامد ثبت‌شده: گواهی DNS-01 منقضی شد و تونل کار کرد ولی کاوشگر گواهی منقضی دید (`CHANGELOG.md:247-254`).

### ۳.۳ استتار و شکل رفتار

| سازوکار | جزئیات | محل |
|---|---|---|
| شکل‌دهی طول رکورد | `shapedConn`: برای هر تکه یک هدف از توزیع ثابت `1400:0.55, 1200:0.08, 900:0.05, 600:0.05, 400:0.05, 250:0.06, 150:0.06, 80:0.06, 40:0.04` (میانگین ۹۹۱٫۴)؛ داده تا `target-4`، تکهٔ آخر/کوچک با صفر پد؛ رکورد روی سیم = هدف + ۲۲ (۹ مقدار ثابت ۶۲..۱۴۲۲)؛ گیرنده قاب >32KiB را خطا می‌داند؛ نمونه‌گیری با `crypto/rand`. سربار حجیم آزمون ۰٫۶۸٪ | `engine/shape.go:11-132`؛ `obfs/shaper.go:24-69` |
| keepalive smux تصادفی | ۴۰۰۰+rand(۴۰۰۰) میلی‌ثانیه، یک‌بار برای هر نشست (ضد ضربان ثابت ۵s) | `engine/mtcp_link.go:270-279` |
| پراکندن دست‌دهی‌ها | دروازه: ۸ هم‌زمان، ۴۰–۱۶۰ms | `engine/dialgate.go:22-41` |
| پد احراز/اثبات | به اندازهٔ GET و سرآیندهای پاسخ HTTP | `tlscarrier/auth.go:62-63` |
| هویت منسجم برای کاوشگر | پاسخ ۴۰۰ دقیق Go، FIN/RST مثل Go، سایت پوششی واقعی | `tlscarrier/server.go:61-67, 94-100`؛ آزمون‌های `server_probe_test.go` |
| بی‌لاگ بودن شکست احراز در سرور | کلید یا ساعت غلط فقط در کلاینت (`ErrOldServer`) دیده می‌شود | `tlscarrier/server.go:123-129` |

- **محدودیت‌های مستند:** تعداد اتصال هم‌زمان قوی‌ترین نشانهٔ رفتاری است (ارزیابی DPI: AUC حدود ۱٫۰؛ طول عمر اتصال ۰٫۹۳؛ تک‌جریانی ۰٫۵۲) و **عمداً بالا نگه داشته شده** (تصمیم صاحب پروژه؛ `obfs/dpi_eval2.py:142-157`؛ `README.md:169-181`). پشتهٔ TLS سرور همان Go است و صفحهٔ پوششی فقط جلوی شمارش انبوه را می‌گیرد (`cmd/hs2/cover.go:28-32`).

### ۳.۴ نشست smux

| پارامتر | مقدار | محل |
|---|---|---|
| `Version` | 2 (کنترل جریان هر جریان) | `engine/mtcp_link.go:269` |
| `MaxFrameSize` = `SmuxFrameSize` | 16KiB (`HS2_TUNE_SMUX_FRAME`) | `:258` |
| `MaxStreamBuffer` = `SmuxStreamBuffer` | 2MiB (`HS2_TUNE_SMUX_STREAMBUF`) | `:262` |
| `MaxReceiveBuffer` = `SmuxSessionBuffer` | 8MiB (`HS2_TUNE_SMUX_SESSBUF`) | `:264` |
| `KeepAliveInterval` / `KeepAliveTimeout` | ۴–۸s تصادفی / 24s | `:278-279` |
| `openCloseTimeout` | 30s | `smux@/session.go:17` |
| صف پذیرش / heap | ۱۰۲۴ / ۱۰۲۴ | `smux@/session.go:15-16` |
| سرآیند قاب | `[ver=2][cmd][len u16 LE][sid u32 LE]`؛ فرمان‌ها SYN، FIN، PSH، NOP، UPD (`[consumed u32][window u32]`)؛ شناسهٔ جریان‌های لبه فرد | `smux@/session.go:486-489`؛ `smux@/stream.go:244-260` |

- **نوشتن:** `writeV2` داده را تا پنجرهٔ همتا به قاب‌های ≤16KiB خرد می‌کند و **هر قاب تا نوشته شدن روی سوکت صبر می‌کند**؛ heap اول CTRL و سپس شمارهٔ سراسری، پس جریان‌ها قاب‌به‌قاب نوبت می‌گیرند (`engine/mtcp_link.go:255-257`). `sendLoop` چون `watchConn` متد `WriteBuffers` ندارد، سرآیند و داده را در یک بافر کپی و با یک `Write` می‌نویسد.
- **خواندن:** بار PSH از سطل نشست کم می‌شود؛ سطل ≤0 ⇒ `recvLoop` دیگر از سوکت نمی‌خواند. گیرنده پس از مصرف ≥1MiB یا در نخستین خواندن، UPD با کلاس **DATA** می‌فرستد و خود `Read` تا نوشته شدنش منتظر می‌ماند؛ روی لینک با آپلود سنگین، خوانندهٔ دانلود پشت قاب‌های داده می‌ایستد.
- **keepalive:** هر ۲۴s بررسی می‌شود؛ اگر از بررسی قبل هیچ قابی نرسیده باشد **و** سطل > 0 باشد نشست بسته می‌شود ⇒ تشخیص ۲۴ تا ۴۸s پس از آخرین قاب؛ **با سطل خالی (خوانندهٔ پارک) هرگز نمی‌بندد** (`smux@/session.go:399-422`).
- **سقف توان هر جریان = پنجره/RTT (محاسبه):** با 2MiB، در RTT ۱۰۰ms حدود ۱۶۰Mbit/s و در ۳۰۰ms حدود ۵۵Mbit/s؛ سطل 8MiB یعنی حداکثر ۴ جریان کاملاً پر روی یک لینک.

### ۳.۵ `watchConn` و `meteredConn`

- `watchConn` (`engine/stream.go:77-123`): نخستین خطای خواندن/نوشتن ⇒ ثبت `why` با پیشوند `read: ` یا `write: ` و بستن conn و نشست در goroutine جدا (`:90-96, 179-184`). توضیح کد: smux خودش فقط با مهلت keepalive متوجه اتصال مرده می‌شود (`:74-76`). `rdCalls` ورود به `Read` را می‌شمارد (نه تکمیل آن؛ برای ناظرها در عمل هم‌ارز است، `:106-108`).
- `describeNetErr` (`engine/stream.go:136-161`): `io.EOF` ⇒ `closed by the other server`؛ `net.ErrClosed` ⇒ `closed locally`؛ timeout ⇒ `timed out (path stalled)`؛ `ECONNABORTED` ⇒ `aborted on this server (…)`؛ `connection reset` ⇒ `reset by the network or the other server`؛ `broken pipe` ⇒ `broken pipe (the other side went away)`؛ unreachable ⇒ `network unreachable`. بدون خطای سوکت: `session ended (keepalive timeout or closed by the other server)` (`engine/stream_kharej.go:161-171`).
- `meteredConn` (`engine/health.go:170-196`): بالای `shapedConn` و زیر smux، پس بار واقعی همهٔ جریان‌ها (کاربر، کنترل، آمار، L3) را بی‌پد می‌شمارد؛ Writeهای بیش از `blockedMin=1ms` به `wrBlocked` می‌روند؛ خطای Write ⇒ `stalls++` (نوشته و هرگز خوانده نمی‌شود).
- **اصلاح:** مهلت نوشتن ۵s حامل (`tlscarrier/carrier.go:27`، برای `WriteRaw`/`SendFrame` خود Carrier) روی مسیر mtcp/l3mtcp **اعمال نمی‌شود**، چون smux روی `RawConn()` می‌نویسد (`:122`). نویسندهٔ لینک فقط با `TCP_USER_TIMEOUT` محدود است. (مهلت ۵s `streamPkt.WriteRaw` در L3 چیز دیگری است و روی جریان smux گذاشته می‌شود.)

### ۳.۶ جریان‌ها و پیام‌ها

بایت نخست هر جریان (`engine/stream.go:33-48`):

| مقدار | نام | باز می‌کند | بعد از بایت نوع |
|---|---|---|---|
| 1 | `kindTCP` | لبه | بایت‌های خام TCP |
| 2 | `kindUDP` | لبه | دیتاگرام‌ها `[len u16 BE][payload]` |
| 3 | `kindL3` | لبه (`OpenRawStream`) | قاب‌های L3 `[ftype][len 3B][pad 3B=0][payload]`؛ `TypeData=1`، `TypePing=3` (`core/frame.go:33-37`) |
| 4 | `kindCtrl` | لبه | ping/pong |
| 5 | `kindPool` | لبهٔ reverse | جریان پیوستهٔ u16 BE |
| 6 | `kindStats` | لبه | دست‌دهی و رکوردها |
| 7 | `kindInfo` | لبه | یک تبادل |
| 8 | `kindTCPPort` | لبه | `[port u16 BE]` سپس TCP |
| 9 | `kindUDPPort` | لبه | `[port u16 BE]` سپس دیتاگرام‌ها |

خروجی نوع ناشناخته را می‌بندد (`engine/stream_kharej.go:254-255`) و لبه از EOF «نسخهٔ قدیمی» را تشخیص می‌دهد.

**`kindCtrl`** (`engine/control.go:33-36, 72-181, 235-256`):
- ping لبه→خروجی: `[seq u64][edgeNanos u64]` (برای هر ping بافر تازه، چون smux نوشتنِ مهلت‌گذشته را با اشاره‌گر به بافر در صف نگه می‌دارد؛ `:135-140`).
- pong: `[seq u64][edgeNanos u64][exitRetrans u64]` (`exitRetrans` از `TCP_INFO` سوکت همان لینک در خروجی). pongها به ترتیب‌اند و هر pong pingهای قبلی را هم پاسخ‌داده حساب می‌کند.
- **آهنگ (اصلاح‌شده، `engine/control.go:102-135`):** تیک ثابت `controlInterval=3s` (`engine/health.go:95`). اگر از تیک قبل ≥96KiB جابه‌جا شده باشد (`heavy`) ⇒ ping روی همان ضرب هر ۳s؛ اگر ≥4KiB (`active`، نه heavy) ⇒ تأخیر تصادفی ۰..۱s پس از تیک، یعنی فاصلهٔ ۲ تا ۴s؛ لینک بی‌کار ⇒ پس از هر ping ۲ تا ۴ تیک سکوت، یعنی هر ۳ تا ۵ تیک (۹ تا ۱۵s). **وقتی ping معلقی هست، سکوت بی‌کاری لغو می‌شود** و در هر تیک ping فرستاده می‌شود (`:115`). حداکثر ۴ ping معلق (`ctrlPending`، `:43`). نوشتن مهلت ۳s دارد و با مهلت‌گذشتن کانال ادامه می‌دهد (`:141-148`)؛ خواندن pong مهلت ۶s (`:152`). روی لینک مرده، چون خواندن تا ۶s و نوشتن تا ۳s مسدود می‌ماند و `Ticker` فقط یک تیک نگه می‌دارد، **آهنگ عملی حدود ۶ تا ۹s** است تا ۴ ping معلق جمع شود (استنتاج؛ روی حکم stuck اثری ندارد چون `ctrlWait` از قدیمی‌ترین ping معلق سنجیده می‌شود).
- **خروجی‌ها:** `ctrlWait` (سن قدیمی‌ترین ping بی‌پاسخ)، `ctrlAnsweredSent`، `rttMicros`، `peerRetrans`، `peerSeen`، `peerLoss` (پنجرهٔ بین دو pong).
- **پایان:** با خطای غیر timeout یا خواندن نیمه‌کارهٔ pong تمام می‌شود و **دوباره باز نمی‌شود** (`:142-143, 156-157`)؛ آن لینک تا پایان عمرش بی‌RTT، بی‌loss دانلود و بی‌stuck می‌ماند.

**`kindStats`** (`engine/stats.go:24-268`):
```
edge→exit [kindStats=6][ver=1]   exit→edge [ver=1][recLen=64][caps]  (bit0=tcp_info, bit1=meter)
edge→exit [seq u32] برای هر poll
exit→edge رکورد recLen بایتی (big-endian):
  0 seq u32 | 4 flags u16 (bit0 chrono valid, bit1 tcp_info ok, bit2 فشار حافظهٔ TCP خروجی)
  6 reserved | 8 monoNs u64 | 16 txBytes | 24 txBlockedNs | 32 busyUs | 40 rwndLimUs | 48 sndbufLimUs | 56 deliveryRate
```
- poll فقط وقتی لینک در تیک ≥16KiB جابه‌جا کرده (`engine/linkmanager.go:1812-1814`)؛ رکورد بلندتر پذیرفته و فقط پیشوندش خوانده می‌شود؛ دست‌دهی و نوشتن مهلت ۵s؛ اگر جریان روی لینک زنده بسته شود ⇒ `statsPending` و بازگشایی پس از ۵s (`statsReopenAfter`). `statsUnsupported` (برای همیشه) فقط اگر EOF (نه timeout) بیاید، لینک ۲۰۰ms بعد هنوز زنده باشد و خروجی به `kindInfo` جواب **نداده** باشد (`engine/stats.go:138-162`)؛ لاگ یک‌باره: `mtcp: the other server does not report link stats (older hs2) — …`. `sndbuf` و `deliveryRate` فرستاده می‌شوند ولی لبه از آن‌ها استفاده نمی‌کند.

**`kindInfo` نسخهٔ ۲** (`engine/peerinfo.go:22-324`):
```
هر طرف: [ver][n u8][n bytes]   v2: maxLinks u16 | caps u8 | flags u8 | count u8 | count × port u16
caps: bit0=capPortTags، bit1=capL3Quiet    flags: bit0 = لبه: UDP هم / خروجی: پنل پیش‌فرض دارد؛ bit1=flagCut
infoMaxPorts = (255−5)/2 = 125 ؛ سقف بیش از 65535 گرد می‌شود
```
- تلاش اول با مهلت ۵s؛ حداکثر ۳ تلاش با فاصلهٔ ۱۰s؛ سپس هر ۱ دقیقه (`:52, 85-89`؛ `engine/stream_iran.go:104-122`). جواب ⇒ `peerInfo` و نیز `lm.exitInfo` (برای لینک‌هایی که جوابشان دیر است)؛ EOF یا نسخهٔ بد ⇒ `infoRefused=true` و آن لینک **هرگز** برچسب پورت نمی‌زند؛ با رفتن آخرین لینک `exitInfo` فراموش می‌شود. هر دو طرف همیشه `capPortTags|capL3Quiet` اعلام می‌کنند.
- **اصلاح (توضیح کد کهنه است):** `engine/stream.go:40` می‌گوید `kindInfo` «display only» است؛ این فقط دربارهٔ **سقف** درست است (`capNote` در `engine/linkmanager.go:609-621`). امروز این تبادل در پنج جا رفتار را عوض می‌کند: (۱) دروازهٔ انتخاب لینک تا پایان تلاش اول (`engine/linkmanager.go:1554`)؛ (۲) برچسب پورت (`engine/stream_iran.go:219-237`)؛ (۳) آهنگ keepalive کانال L3 (`engine/peerinfo.go:74-80`)؛ (۴) هدف اپیزود refill در لبهٔ معکوس (`engine/refill.go:123-136`)؛ (۵) سقف خروجی در `sweepReverse` (`engine/linkmanager.go:1187, 1313-1321`).

**`kindPool`** (`engine/exit_pool.go:29-34, 470-584`): پس از بایت نوع، جریانی از u16 BE (تعداد لینک مطلوب = `ctlTarget()`). خروجی هیچ‌وقت روی آن نمی‌نویسد؛ خروجی قدیمی آن را فوراً می‌بندد و لبه «رد» ثبت می‌کند (`markPoolRefused`).

**جریان کاربر و مسیریابی پورت** (`engine/routes.go:13-155`؛ `engine/stream_iran.go:212-237`؛ `engine/stream_kharej.go:199-244`):
- لبه فقط **شمارهٔ پورت** را می‌گوید؛ خروجی با جدول خودش تصمیم می‌گیرد: ورودی `port_map` (`P` یعنی `127.0.0.1:P`، یا `P=host:port`) ← `expose` ← رد با لاگ حداکثر یک بار در دقیقه برای هر پورت/پروتکل. دلیل امنیتی ثبت‌شده: ایرانِ نفوذشده نمی‌تواند خارج را به نشانی دلخواه (مثل `127.0.0.1:22`) بفرستد (`engine/routes.go:25-29`).
- برچسب‌زنی: اگر پاسخ info همین لینک (یا اگر هنوز نرسیده و رد نشده، پاسخ استخر) v2 با `capPortTags` باشد، **همهٔ** پورت‌ها برچسب می‌خورند (حتی نانگاشته‌ها)، پس ویرایش `port_map` خارج پس از ری‌استارت خارج اثر می‌کند بی‌آنکه ایران عوض شود.
- `hs2 check`: لبهٔ mtcp بی‌`forward_ports` ⇒ خطا؛ لبهٔ l3mtcp/tls بی‌پورت ⇒ فقط هشدار («pure routed tunnel»)؛ خروجی mtcp/tls بی‌`expose` و بی‌`port_map` ⇒ خطا، خروجی l3mtcp ⇒ هشدار (`cmd/hs2/check.go:217-276, 370-410`).

### ۳.۷ تنظیم سوکت لینک

| تنظیم | مقدار | محل | توضیح |
|---|---|---|---|
| `TCP_NODELAY` | روشن | `tlscarrier/tune_linux.go:37` | هر دو سمت |
| `TCP_NOTSENT_LOWAT` | 32KiB (`HS2_TUNE_NOTSENT`) | `:19, 44` | آزمایشگاه: ۱۶–۳۲KiB بهترین؛ ≥۶۴KiB تأخیر افزود؛ خاموش ۳ تا ۷ برابر بدتر (`:12-18`). معنای `wrBlocked` و فشار آپلود به آن وابسته است |
| `TCP_USER_TIMEOUT` | 20000ms | `:22, 46` | هر دو سمت (`carrier.go:190`؛ `server.go:74`)؛ دادهٔ تأییدنشدهٔ ۲۰s ⇒ خطای سوکت |
| `TCP_CONGESTION` | `bbr` یا cc طرح tune (یا `HS2_TUNE_CC`) | `:29, 47-49`؛ `cmd/hs2/main.go:357-359` | حتی با `tuning.mode=off` روی سوکت‌های تونل اعمال می‌شود |
| keepalive TCP | ۳s فقط `TCP_KEEPIDLE` سمت شماره‌گیر | `tlscarrier/carrier.go:186-189` | بخش ۳.۲ بند ۴ |
| MPTCP | خاموش (`godebug multipathtcp=0` در `go.mod:7` و `SetMultipathTCP(false)`) | `engine/listen.go:21-41` | سوکت پذیرفتهٔ MPTCP `tcp_notsent_lowat` را نادیده می‌گرفت؛ p99 echo هشت ثانیه در برابر ۰٫۵ (`CHANGELOG.md:827-861`) |
| خطاهای `setsockopt` | بی‌صدا نادیده | `tlscarrier/tune_linux.go:31-51` | — |

### ۳.۸ حالت معکوس (reverse)

**لبهٔ معکوس** (`acceptReverseLinks`، `engine/stream_reverse.go:47-101`):
- یک `sampler` مشترک برای همهٔ لینک‌های پذیرفته؛ `srv.Handle` همان احراز.
- **سقف پذیرش:** اگر لینک‌های **زنده** ≥ `2*max+8` باشد، لینک رد می‌شود؛ ابتدا ۵s نگه داشته می‌شود تا خروجی فوراً دوباره نزند؛ لاگ حداکثر دقیقه‌ای: `mtcp: refused %d reverse link(s)%s (latest from %s): this server holds at most %d (twice its max_links %d + %d) — check the Kharej server's min_links/max_links` (`:35-41, 68-83`). (شمردن لینک‌های مرده طوفان ۴۶٬۷۲۱ دست‌دهی ساخته بود.)
- `newEdgeLink` ⇒ `lm.AddLink`؛ اگر serving ≥ هدف باشد لینک «spare» به دنیا می‌آید (`bornSpare`، retiring) و ۳۰s (`bornSpareGrace`) بسته نمی‌شود، چون شاید جایگزین لینکی باشد که مرده ولی لبه هنوز نفهمیده (`engine/linkmanager.go:409-437, 308`). spare هم L3 می‌گیرد.
- با بسته شدن نشست ⇒ فوراً `DropLink` و لاگ `mtcp: reverse link %d from %s down: %s (now %d)` (`:474-497`).
- لبهٔ معکوس شماره نمی‌گیرد؛ همان autopilot هدف را تعیین و با `kindPool` به خروجی می‌فرستد.

**خروجی معکوس** (`runKharejReverse` و `exitPool`، `engine/stream_reverse.go:109-192`؛ `engine/exit_pool.go`):
- اندازهٔ اولیه `RevLinks = WarmSize(min,max)` (۸ بریده‌شده در بازه)؛ فایل warm را نمی‌خواند (`cmd/hs2/main.go:529-530`).
- هر **slot** (`runSlot`، `engine/exit_pool.go:326-413`): `retireIfOver` ← `waitTurn` ← `gate.acquireIf` (با شرط «هنوز لازم است») ← شماره‌گیری ← `serveReverseLink` تا پایان لینک ← اگر بالای هدف بود بازنشسته، وگرنه شماره‌گیری دوباره: اگر لینک ≥۳۰s (`slotStableAfter`) زنده بوده پس از `jitterDur(500ms)` (۲۵۰–۵۰۰ms)، وگرنه با backoff دوبرابرشونده `[d/2,d)` از ۵۰۰ms تا ۸s.
- **قطعی** (خطای شماره‌گیری با `live==0`): فقط یک slot «scout» با شماره‌گیر کوتاه (connect ۲s، `RevDialScout`) و backoff ≤۲s تلاش می‌کند و بقیه روی `upCh` منتظرند؛ لاگ آغاز `mtcp: no link up to the edge — dials fail (%v); one slot keeps trying (every ≤%s), the other %d wait for it` و پایان `mtcp: a link to the edge is back after %s with none up (%d dial(s) failed meanwhile); the other slots redial now` (`:259-323`). اولین لینک پس از قطعی <۳s (`regress300_test.go`).
- اگر همهٔ لینک‌ها بروند و هدف بالاتر از اندازهٔ اولیه بوده، هدف به اندازهٔ اولیه برمی‌گردد (`:420-432`).
- **کوچک شدن:** خروجی **هرگز خودش لینکی را نمی‌بندد**؛ فقط هدف را پایین می‌آورد. لبه لینک خالی را می‌بندد و slot چون بالای هدف است بازنشسته می‌شود؛ لاگ‌ها «retired — closed by the edge while above its target (pattern shrinking)» را از «lost (…) — not redialed» جدا می‌کنند (`:155-160, 378-397`).
- **سقف:** هدف به `[min, min(max, سقف گزارش‌شدهٔ لبه)]` محدود می‌شود (`:144-168`).
- لینکی که بدون خطای سوکت و بدون لغو ctx تمام شود دلیلش `no data from the edge for 24s (keepalive timeout — path stalled)` ثبت می‌شود (`engine/stream_reverse.go:177-190`).

**کنترل استخر (`openPoolCtl`، `engine/exit_pool.go:448-584`؛ `engine/linkmanager.go:695-737, 1335-1349`):**
- `ctlTarget = T + ctlDrain` (تعداد draining که جایگزینشان خواسته شده).
- ارسال در شروع؛ با هر تغییر: روی **دو لینک قدیمی‌تر زنده** («سریع») فوری و روی بقیه با تأخیر تصادفی ≤۱٫۵s (`poolCtlSpread`)؛ تازه‌سازی دوره‌ای با `jitterAround`: **۲٫۴–۳٫۶s** روی دو لینک سریع و **۲۴–۳۶s** روی بقیه (اصلاح؛ `engine/exit_pool.go:528-536`).
- مهلت نوشتن ۳s؛ هر خطای نوشتن ⇒ پایان همیشگی همان حلقه؛ EOF روی لینک زنده ⇒ `markPoolRefused`.
- `servePoolCtl` در خروجی: هر عدد ⇒ `setTarget` (محدود به `bounds()`).

---

## ۴. LinkManager و انتخاب لینک

### ۴.۱ نقش و ساختار

- `LinkManager` فقط در **لبهٔ حالت جریانی** (mtcp، l3mtcp، tls) ساخته می‌شود (`engine/stream_iran.go:69, 72`). کارهایش: نگه‌داشتن N لینک، پخش اتصال‌های تازه، سنجاق کردن هر اتصال به لینکش، اندازه‌گیری سلامت و فشار، اجرای تصمیم autopilot، بازنشسته‌کردن و بستن مازاد، و جایگزینی make-before-break (`engine/linkmanager.go:18-35`). autopilot «مغز» است و `LinkManager` «محرک» (`engine/autopilot.go:11-15`).
- `NewLinkManager` (`engine/linkmanager.go:324-347`): `min≥1`، `max≥min`، `perLink=8`، `target=warmSize`، `drainIdle=310s`، چهار `burstLog` (لاگ تاشونده: بیش از ۸ خط در ۱۰s ⇒ خلاصه، `engine/burstlog.go:17-73`).
- مهم‌ترین فیلدهای `managedLink` (`engine/linkmanager.go:229-296`): `users` (اتمیک)؛ شمارنده‌های قبلی برای دلتا؛ رگه‌های loss (`upStreak`، `dnStreak`، `upBadAt`، `dnBadAt`)؛ `stuckStreak`، `stuck`؛ `degraded`، `draining`، `drainSince`، `drainReplace`؛ `retiring`، `retireSince`، `servingSince`؛ `upHist`/`dnHist` (۳ بیت)، `pressed`؛ `suspect`، `lastRx`، `rxSeen`؛ `rates[5]`، `doms[3]`، `rate`، `rate10`، `sustained`؛ `flowing`، `open`، `recent`، `lastByte`؛ `picks`، `pickHist[2]`؛ `poolRefused`، `ctlLive`، `bornSpare`. فیلدهای `dead`، `goodput` و `lossFrac` نوشته ولی خوانده نمی‌شوند؛ `mtcpLink.dead` هرگز true نمی‌شود.
- **تعریف serving:** `!retiring && !degraded && !draining && Alive() && !suspect` (`engine/linkmanager.go:299-301`).

### ۴.۲ ترتیب تیک (هر `healthTick=2s`، `engine/health.go:18`)

- **مستقیم** (`Run`، `engine/linkmanager.go:546-583`): `reap → sampleHealth → heal → autoscale (decideTarget + reconcile) → drainTick → publishStats`. پر شدن اولیه: `min(warm, ceil(warm/4)+gateInflight)` شماره‌گیری بی‌درنگ (`:564`).
- **معکوس** (`runAccept`، `:1124-1140`): `sampleHealth → reconcile(decideTarget) → drainTick → sweepReverse → publishStats`؛ کار `reap`/`heal` را `sweepReverse` و `DropLink` انجام می‌دهند؛ `reconcile` شماره نمی‌گیرد (`:826-828`).
- حکم‌های loss/stuck که در `sampleHealth` صادر می‌شوند در **همان تیک** به `heal`/`sweepReverse` می‌رسند؛ نمونهٔ autopilot پیش از `heal` ساخته شده است.

### ۴.۳ انتخاب لینک برای اتصال تازه (`Pick`)

- `Pick` (`engine/linkmanager.go:1514-1523`): زیر **قفل نوشتن** `m.mu`، `pickLocked(0)` و `placeLocked` (`users++`، `picks++`، `m.users++`) و تابع `release` یک‌باره (`sync.Once`).
- **لایه‌ها** (`pickLocked`، `:1548-1586`): لایهٔ ۰ = serving؛ لایهٔ ۱ = retiring سالم (نه degraded/draining/suspect)؛ لایهٔ ۲ = **هر لینک زنده، حتی degraded/draining/suspect** (`:1560-1569`). در همهٔ لایه‌ها: لینک مرده رد؛ اگر `gateInfo` و `infoDone` نشده ⇒ رد (`:1554`)؛ اگر `limit>0` و `users≥limit` ⇒ رد (کلاهک refill).
- **کلید مرتب‌سازی** (`pickKey`، `:1470-1484`): اول لینک بی‌فشار؛ سپس کمترین `load = flowing + picks`؛ سپس کمترین `users`؛ تساوی کامل ⇒ تصادفی (reservoir، `:1571-1579`).
- **کلاهک انفجار** (`newPickKey`، `:1486-1497`): لینکی که در ۳ نمونهٔ اخیر (`pickWindow=3`، حدود ۴–۶s) ≥ `max(1, perLink/2)` اتصال تازه گرفته (با `per_link=8` یعنی ۴) «pressed» حساب می‌شود، چون فشار چند ثانیه دیر اندازه‌گیری می‌شود.
- **ورودی‌هایی که اصلاً وجود ندارند:** RTT، loss، `ctrlWait`، نرخ.
- **دروازهٔ info** (`gateInfo=true`): لینک تا پایان **نخستین** تلاش `kindInfo` کاربر نمی‌گیرد. **اصلاح:** این «حداکثر ~۵s» نیست؛ `exchangeInfo` اول `OpenRawStream` می‌کند که SYN آن تا `openCloseTimeout=30s` پشت `sendLoop` می‌ماند و بعد مهلت ۵s می‌گذارد، پس روی لینکی که نویسنده‌اش گیر است تا **حدود ۳۵s** (`engine/peerinfo.go:267-307`؛ `smux@/session.go:145, 526`).
- **`pickWait`** (`engine/stream_iran.go:183-210`): تا ۴۰ دور؛ `pickHeld` (در اپیزود refill) یا `Pick`؛ بی‌لینک ⇒ خواب ۱۵۰ms؛ در اپیزود تا ۱۰s روی کانال انتظار. UDP بی‌hold.

### ۴.۴ `reconcile` (محرک T، `engine/linkmanager.go:767-862`)

1. دسته‌بندی: serving و retiring؛ لینک مرده، degraded، draining و suspect **در هیچ‌کدام نیستند**.
2. **رشد:** اگر serving<T و retiring هست ⇒ اول بازنشسته‌ها برمی‌گردند (بیشترین `open`، سپس تازه‌ترین `lastByte`)؛ `servingSince=now` (برای انتساب پروب).
3. **کوچک‌شدن:** اگر serving>T ⇒ مازاد retiring می‌شود؛ قربانی‌ها به ترتیب کمترین `flowing` ← `recent` ← `open` ← `rate10` (زودتر خالی‌شونده‌ها)؛ `pressed=false`.
4. لاگ‌ها: `link(s) %s back in service (…)`، `link(s) %s retiring — no new connections; each closes once its connections end (…)`، `dialed %d link(s) — %d up, %d of %d serving`، و هر ۳۰s `want %d serving links, only %d up — dials failing (peer down or path blocked): %s` (عدد دوم در واقع serving است).
5. **فقط مستقیم — صف شماره‌گیری:** `n = T − S − inflight`، حداکثر `ceil(T/4)`، حداکثر `dialRoom − inflight`، حداکثر `ceil(T/4) + gateInflight − inflight`. `dialRoomLocked = min(max − slotted, max + drainHeadroom(max) − len(links))` که در آن **هر لینک غیر draining، از جمله suspect،** «slot» است (`:2196-2208`).

### ۴.۵ `queueDial` و دروازه (`engine/linkmanager.go:865-950`؛ `engine/dialgate.go:22-77`)

- `dialing++` پیش از goroutine؛ `valid()` = epoch عوض نشده و (جایگزین است یا `wantsDial`: جا هست و serving<T).
- `gate.acquireIf(ctx, valid)`: پیش از رزرو فاصله و پس از خواب دوباره `valid` بررسی می‌شود؛ جای دروازه تا پایان دست‌دهی نگه داشته می‌شود.
- **شکست:** `failStreak++`؛ اگر ≥۳ (`dialFailRun`) یا هیچ لینک زنده‌ای نیست ⇒ `dialEpoch++` و همهٔ شماره‌گیری‌های صف‌شده بی‌تلاش رها می‌شوند. (رها کردن با یک شکست، رمپ با ۱–۵٪ خطا را می‌ایستاند.)
- **موفقیت:** `failStreak=0`، افزودن زیر قفل + `noteArrivalLocked` (شروع احتمالی refill)، لاگ (`dialed replacement link %d (make-before-break)` برای جایگزین)، `OnLink`.
- **مستقیم هیچ backoff نمایی یا scout ندارد**؛ بازتلاش با تیک ۲s است (برخلاف `exitPool`).
- راهنمای short-lived: سه لینک پیاپی با عمر <۲۰s ⇒ یک بار `%d links in a row died within %s of coming up — this path lets TCP start and then kills it (…); the tun over icmp transport (tun → icmp) does not use TCP on the wire` (`:1440-1462`).

### ۴.۶ بستن بازنشسته‌ها (`drainTick`، `engine/linkmanager.go:959-1117`)

- **نگهبان معکوس:** `now − targetDropAt ≥ 4s` (`retireAfterDrop`، `engine/health.go:90`)، دست‌کم یک لینک ≥۴s که pool-control را رد نکرده، و بیرون از churn hold (`:963-972`).
- لینک retiring زنده (نه draining) بسته می‌شود اگر: `users==0 && Active()==0` (**جریان‌های خام از جمله L3 شمرده نمی‌شوند**)، در معکوس سن ≥۴s، و اگر `bornSpare` سن ≥۳۰s (`:983-988`). از استخر زیر قفل بیرون می‌رود و بیرون از قفل با لرزش ۵۰–۲۵۰ms (`closeJitter`) بسته می‌شود.
- **اصلاح:** سقف بستن در هر تیک `closesPerTick(R) = min(max(2, ceil(R/32)), 8)` است، یعنی **۲ تا ۸** (نه «حداکثر ۲»؛ «۲» فقط تا ۶۴ لینک retiring درست است) (`:1062-1064`).
- **بازپس‌گیری بی‌کار** (`reclaimIdle`، `:1082-1117`): اگر `drainIdle>0 && open>0` (یک goroutine برای هر لینک): جریان‌هایی که `drainIdle` (پیش‌فرض ۳۱۰s، کمی بالای `connIdle` ۳۰۰s پنل xray) بی‌بایت بوده‌اند، حداکثر ۱۶ تا در هر گذر، با **FIN**؛ پس از `retireForce=20m` بازنشستگی، جریان‌های «جاری‌نبودن» هم؛ جریانی که بین انتخاب و بستن تکان خورده بخشیده می‌شود. جریان flowing هرگز بسته نمی‌شود.
- لاگ «held»: پس از ۱۵ دقیقه و سپس هر ساعت (`heldLogFirst`/`heldLogEvery`).

### ۴.۷ تخلیهٔ لینک degraded (`heal`، `engine/linkmanager.go:2089-2182`؛ فقط مستقیم)

1. هر `degraded && !draining` ⇒ `draining=true`، `drainReplace=!retiring`، `retiring=false`، `drainSince=now`.
2. برای هر لینک serving که degraded شد: اول یک retiring زندهٔ غیر degraded با بیشترین `open` برمی‌گردد (**suspect بررسی نمی‌شود**، `:2103-2108`)؛ لاگ `link(s) %s back in service to replace a degraded link`.
3. کسری با شماره‌گیری جایگزین (`replacement=true`؛ می‌تواند از max بگذرد): حداکثر `drainHeadroom(max)` در تیک و کل لینک‌ها ≤ `max + headroom`.
4. پله‌ها (`drainStepLocked`، `:1216-1239`) در هر تیک: کاربر صفر ⇒ بستن فوری؛ سن >۹۰s ⇒ بستن با همهٔ کاربران؛ سن >۴۵s **یا stuck** ⇒ `reclaimStalled`: جریان‌هایی که ≥۱۵s بی‌بایت بوده‌اند با FIN و **موازی** (فاصلهٔ شروع ۲۰ms) بسته می‌شوند (`:1361-1402`).
5. **پس‌دادن جا** (`slotsBackLocked`، `:1262-1309`): اگر serving + retiring + در حال شماره‌گیری + جا < هدف، قدیمی‌ترین drainingهایی که از ۴۵s گذشته (یا stuck) بسته می‌شوند.
6. `drainHeadroom(n) = max(2, ceil(n/8))` (`:1253`): max=32 ⇒ ۴؛ ۴۸ ⇒ ۶؛ ۶۴ ⇒ ۸؛ ۳۰۰ ⇒ ۳۸. لینک draining خارج از max شمرده می‌شود.
- **degraded/draining برگشت‌ناپذیرند**؛ هیچ مسیری آن‌ها را false نمی‌کند.

### ۴.۸ ویژه‌های حالت معکوس

- **`sweepReverse`** (`engine/linkmanager.go:1145-1210`): مرده‌ها بسته؛ degraded ⇒ draining (`drainReplace=!retiring`)؛ پله‌ها؛ `slotsBackLocked` با room = سقف خروجی − تعداد (اگر `growable`)؛ اگر `ctlDrain` عوض شد ⇒ `notifyCtl` **پیش از** بستن‌ها. لاگ‌ها: `reverse link %d degraded — draining; the exit is asked for a replacement` یا `… (it was retiring: not replaced)`.
- **نگهبان churn** (`noteSurplusArrivalLocked`، `:442-470`): ۳ ورود مازاد، هر کدام ≤۱۰s پس از یک retire-close، در پنجرهٔ ۳ دقیقه ⇒ ۱۰ دقیقه هیچ retire-close؛ لاگ `the exit redials links this server retires (check the exit's min_links) — keeping %d up for %s`.
- **`growable`** (`:1995-2000`): `!accept || aged==0 || poolOK>0`؛ اگر همهٔ لینک‌های ≥۴s کنترل استخر را رد کرده باشند (خروجی قدیمی) ⇒ استخر قابل رشد نیست و T به `S+R` محدود می‌شود؛ لاگ یک‌باره `the exit has no pool control (older hs2) — the link count is fixed by its rev_links until it is upgraded`.

### ۴.۹ نگه‌داشت refill (`engine/refill.go:62-351`)

- **شروع:** لینکی برسد در حالی که هیچ لینک زنده‌ای نیست (`aliveLocked()==0`)، `target ≥ 2`، و بیرون از `quietTill` (یعنی پس از شروع یا قطعی کامل).
- **عمل:** اتصال‌های TCP تازه در صف می‌مانند تا لینکی «با جا» پیدا شود؛ سقف هر لینک `max(perLink, ceil((users+queue)/T))` (سهم منصفانهٔ ثابت؛ `:141-145`)؛ UDP نگه داشته نمی‌شود. تیک ۱۰۰ms.
- **پایان:** complete (S ≥ T، یا در معکوس سقف خروجی)، limit (۱۰s)، stalled (۳s بی‌لینک تازه). پس از limit/stalled تا ۱ دقیقه اپیزود تازه نیست. اعداد سنجیده: قطعی با ۲۴۰۰ اتصال و ۳۰۰ لینک: پرترین لینک ۲۴۰۰ → ۲۴؛ راه‌اندازی شبیه تولید (۶۰۰۰ اتصال، ۶۳ لینک): ۷۸۱ → ۹۶.
- لاگ‌ها: `refill: %d of %d links up — new connections wait (%s at most) for a link with room, …` و سه خلاصهٔ پایان؛ و `links open at the dial pace, about %d a second, so %d take about %ds — expected, not a fault` یا `links are coming slower than the dial pace …`.

### ۴.۱۰ شروع گرم

- `WarmSize(min,max)` = `warmStartLinks=8` بریده‌شده در `[min,max]` (`engine/health.go:85`؛ `engine/linkmanager.go:742-751`).
- فایل `/run/hs2/<مسیر-پیکربندی>.warm` (`cmd/hs2/status.go:192-246`): نوشتن فقط پس از ۱ دقیقه کارکرد (تا حلقهٔ کرش مقدار بالا را تمدید نکند) و با تغییر هدف یا هر ≥۱ دقیقه؛ خواندن فقط اگر ≤۱۵ دقیقه عمر دارد؛ **اصلاح:** فقط وقتی به کار می‌رود که از `WarmSize(min,max)` بیشتر باشد (نه «بالاتر از ۸»)؛ فقط اندازهٔ شروع را **بالا** می‌برد؛ گیره در `[min,max]` (`cmd/hs2/main.go:280-293`). فقط لبه می‌خواند. reboot (tmpfs) ⇒ شروع سرد.
- لاگ: `link pool: coming up at %d links, the size it had before this restart (the autopilot resizes it from there)`.

### ۴.۱۱ ماشین حالت `managedLink`

```
              (dial موفق / AddLink)
                     │
        ┌────────────▼─────────────┐
        │ تازه: تا infoDone قابل pick نیست │  (≤۵s، یا تا ~۳۵s اگر نویسنده گیر باشد)
        └────────────┬─────────────┘
   AddLink با S≥T    │
 ┌── born spare ◄────┤
 │ (retiring+bornSpare، ≥۳۰s)
 ▼                   ▼
retiring ◄──reconcile shrink── serving ──(pressed: فقط اولویت پایین‌تر)
 │ ──reconcile grow / heal──►    │  ▲
 │                    ۱۲s بی‌دریافت (rxSeen)    │ نخستین بایت (بی‌لاگ)
 │                               ▼  │
 │                           suspect (بیرون از لایه‌های ۰ و ۱؛ در S نیست؛ در dialRoom هست)
 ├─ خالی + نگهبان‌ها ⇒ drainTick: بستن (۲..۸ در تیک)
 └─(حکم loss/stuck)─► degraded ─(همان تیک heal/sweepReverse)─► draining
                                     │
   users==0 ⇒ بستن | >45s یا stuck ⇒ بستن بی‌حرکت‌ها (≥15s) | >90s ⇒ بستن | سقف+کمبود ⇒ slotsBack
                                     ▼
                                   بسته
هر حالت ──(Alive()=false: خطای سوکت/USER_TIMEOUT/keepalive smux)──► reap / sweepReverse / DropLink
```

- **اصلاح:** پرچم و لاگ suspect **بی‌شرط** است (`engine/linkmanager.go:1735-1744`) و برای لینکی که stuck/degraded شده و در حال تخلیه است **هم** می‌آید؛ اثر عملی کم است چون آن لینک از پیش بیرون از لایه‌های ۰ و ۱ است.
- **چه چیزی لینک را می‌کشد:** فقط مرگ نشست/سوکت (TCP، keepalive smux، خطای I/O)، بستن retiring خالی، پایان تخلیهٔ degraded، `closeAll`. **چه چیزی فقط کنار می‌گذارد:** suspect، retiring، degraded/draining، pressed (فقط اولویت)، نبود `infoDone`.

### ۴.۱۲ پایش (`Stats`/`publishStats`/`PoolStats`)

- `PoolStats` (`engine/linkmanager.go:2216-2372`) فقط افزایشی است (خوانندهٔ قدیمی نمی‌شکند): links، target، min/max، serving/retiring، users، mbit، counted، phase، reason، `CapMbit`، `PeakMbit`، `NextProbeS`، `Pressed`، `Saturated`، held، refill، exit stats، `PeerMax`، `Routes`. **مشاهده:** `Stats()` لینک suspect را در `Serving` می‌شمارد در حالی که `countsLocked` نمی‌شمارد؛ شمار suspect/degraded/draining در `PoolStats` نیست.


---

## ۵. autopilot: الگوریتم کامل با عددها

### ۵.۱ جایگاه، ورودی و تعریف‌های پایه

- `autopilot` یک **هستهٔ تصمیم خالص** است: قفل، سوکت و ساعت ندارد. هر ۲ ثانیه یک `apSample` می‌گیرد و یک عدد بیرون می‌دهد: **T = تعداد لینک‌های serving** (لینک‌هایی که اتصال تازه می‌پذیرند) (`engine/autopilot.go:11-21`). `LinkManager` محرک آن است: un-retire، dial و retire.
- **فقط روی لبه (ایران)** اجرا می‌شود، در هر دو حالت. فراخوان: `LinkManager.decideTarget` (`engine/linkmanager.go:590-607`)، در حلقهٔ مستقیم (`:574-581`) و معکوس (`:1132-1138`). در معکوس، لبه T را از کانال `kindPool` به خروجی می‌فرستد و خروجی فقط پیروی می‌کند (`engine/exit_pool.go:14-30, 161-185`). `pin` (فقط آزمون‌ها) آن را دور می‌زند (`engine/linkmanager.go:591-595`).
- **همین کد** پول datagram (`dgPool`، dgtun) را هم اندازه می‌گیرد، با تعریف دیگری از pressed (`engine/dgpool.go:1198, 1390`). هر تغییری در `autopilot.go` هر دو را عوض می‌کند.
- **ورودی** (`apSample`، `engine/autopilot.go:209-216`): `now`، `links []apLink`، `G` (بایت بر ثانیهٔ کل)، `flowing`، `open`، `growable`. هر `apLink` (`:195-206`): `id, serving, retiring, servingSince, pressed, rate, rate10, sustained, flowing, open`. سازنده: `sampleHealth` (`engine/linkmanager.go:1619-2024`).
- **خروجی** (`apDecision`، `engine/autopilot.go:219-224`): `target, phase, reason, note`. `note` یک خط لاگ `mtcp: pattern …` است؛ `phase` و `reason` به مانیتور زنده می‌روند (`PoolStats.Phase/Reason`، `engine/linkmanager.go:2297-2309`).
- **سازنده** (`newAutopilot`، `engine/autopilot.go:259-277`): `min ≥ 1`، `max ≥ min`، `perLink` پیش‌فرض ۸، `capMax = max(256, 4·max)`، و **T اولیه = `warmSize(min,max)`** یعنی ۸ بریده‌شده در `[min,max]` (`engine/linkmanager.go:742-751`). `SetWarm` (فایل warm) فقط می‌تواند آن را بالاتر ببرد.

**تعریف‌هایی که همه‌جا به کار می‌روند:**

| مفهوم | تعریف دقیق | path:line |
|---|---|---|
| **flowing** (یک جریان کاربر) | EWMA نرخ با τ=۱۰s ≥ 2KiB/s **و** بایتی در ۶s اخیر؛ **یا** ≥256B/s در هر ۳ نمونهٔ آخر. handshake و heartbeat هرگز flowing نیستند | `engine/health.go:41-48`؛ `engine/mtcp_link.go:97-130` |
| **rate / rate10 / sustained** (یک لینک) | `rate = (dRd+dWr)/secs`؛ `rate10` = میانگین ۵ نرخ آخر (۱۰s)؛ `sustained` = **کمینهٔ** ۳ `dom` آخر (۶s) که `dom = max(dRd,dWr)/secs` | `engine/linkmanager.go:1777-1790` |
| **فشار آپلود** | `perTick(dWr) ≥ 16KiB && dBlocked/dt ≥ 0.5 && upRwnd < 0.5` (`upRwnd = Δrwnd_limited/Δbusy_time`، فقط با chrono معتبر)؛ سه بیت آخر در `upHist` | `engine/linkmanager.go:1797-1799` |
| **فشار دانلود** | از رکورد `kindStats` خروجی (seq تازه): همان سه شرط روی `tx`، `txBlocked`، `rwnd`؛ فاصلهٔ رکوردها > `statsGap`=۷s یا شمارندهٔ عقب‌رفته ⇒ پایهٔ تازه | `engine/linkmanager.go:2032-2064` |
| **pressed** | `serving() && (popcount(upHist) ≥ 2 \|\| (statsOK && popcount(dnHist) ≥ 2 && now−lastRecAt ≤ 6s))` — یعنی ۲ از ۳ نمونه | `engine/linkmanager.go:1804-1806` |
| **poll آمار** | فقط وقتی لینک ≥16KiB در تیک جابه‌جا کرده باشد ⇒ دانلود pressed روی لینک تازه‌بیکارشده ۶s بعد کهنه می‌شود | `engine/linkmanager.go:1812-1814` |
| **growable** | `!accept \|\| aged == 0 \|\| poolOK > 0` ⇒ در معکوس، اگر همهٔ لینک‌های ≥۴s کانال pool را رد کرده باشند (خروجی قدیمی)، پول قابل رشد نیست | `engine/linkmanager.go:1995-2000` |

- `perTick(b) = b × 2s / dt` (نرمال‌سازی به تیک ۲ ثانیه) و `dt` واقعی بین دو نمونه است (`engine/linkmanager.go:1621-1629, 1682`).
- اولین نمونهٔ هر لینک فقط پایه است: `pressed=false` و نرخ صفر؛ به G افزوده نمی‌شود ولی `flowing` آن افزوده می‌شود (`engine/linkmanager.go:1745-1759`).
- لینک degraded یا draining در `s.links` هست ولی با `serving=false, retiring=false`؛ پس در S و R نیست ولی نرخش در G هست (`engine/linkmanager.go:1706-1710, 2066-2073`).
- **ترافیک TUN:** جریان L3 خام است و در `flowing` شمرده نمی‌شود، ولی بایت‌هایش در `rate`، `G` و `wrBlocked` هست (`engine/mtcp_link.go:59-73, 97-128, 183`؛ `engine/health.go:170-196`). (تأیید ردیف ۶۳ نقشهٔ ۹۱.)

### ۵.۲ `decide`: اندازه‌گیری‌های پیش از مراحل (`engine/autopilot.go:340-467`)

1. **قطعی کامل** (`len(s.links)==0`): T و تاریخچه دست‌نخورده می‌مانند؛ دلیل `no link is up — waiting for links` (`:343-350`؛ آزمون `TestAutopilotOutageKeepsSize`).
2. شمارش `S` (serving)، `R` (retiring)، `P` (serving و pressed). برای هر لینک serving+pressed با `sustained>0` ⇒ `noteCap` (`:353-367`).
3. `shortTick = P ≥ 1 && S−P < spare(P)` (`:368`) و افزودن به `hist` (حداکثر ۳۰ خانه = ۶۰s).
4. `isShort` = short در ≥۳ از ۵ تیک آخر (`:374-380`).
5. `fl5` = **کمینهٔ** `flowing` در ۵ تیک آخر (۱۰s)؛ `fl60` = **بیشینهٔ** `flowing` در ۶۰s؛ `p60` = بیشینهٔ P در ۶۰s؛ `shortIn60`؛ `gPeak` = بیشینهٔ **میانگین دوتیکی** G در ۶۰s («یک جهش تک‌تیکی قله نیست») (`:381-409`). این هیسترزیس عمدی است: رشد کف کند و محافظه‌کار، نگه‌داشتن سخاوتمندانه.
6. `fGrow = ceil(fl5/perLink)`؛ `fHold = ceil(fl60/perLink)` (`:410-411`).
7. `U = clamp(max(fl60+2, p60+spare(p60)))` = لینک‌هایی که جریان‌های فعال واقعاً می‌توانند به کار ببرند (`:416`).
8. `cCap = capEstimate(now)`؛ اگر `cCap>0 && gPeak ≥ 16KiB/s` ⇒ `needBW = ceil(gPeak / (0.7·cCap))` (`:417-421`).
9. `needSat = p60 + spare(p60)`؛ `holdN = hold.n` اگر hold معتبر باشد و `gPeak ≥ 0.6·hold.g` (`:422-426`).
10. `need = min(max(needSat, needBW), U)` ⇒ `max(need, holdN)` (hold به U محدود نمی‌شود) ⇒ `max(need, fHold)` ⇒ **`H = clamp(need)`** = اندازه‌ای که تقاضای اخیر لازم دارد (`:427-443`).
11. **بازنشانی عقب‌نشینی** اگر `k>0` (`:448-461`): در حین CONFIRM هیچ؛ وگرنه اگر میانگین G در ۱۰ تیک آخر (۲۰s) > `1.3·fail.g` **یا** `fl5 > int(1.5·fail.flows)+2` ⇒ `k=0` و `next = min(next, now+30s)`؛ وگرنه پس از ≥۳۰ دقیقه از شکست ⇒ `k=0`. (یک جهش ۴ ثانیه‌ای ۱٫۴× بازنشانی نمی‌کند: `TestBackoffNotResetBySpike`.)
12. متن `why`: `%d active of %d open connections, %d of %d serving links at their limit, peak %.1f Mbit/s` + ` (one link carries ~%.1f Mbit/s)` (`:463-467`).

**`spare(p)`** (`engine/autopilot.go:284-296`): صفر اگر p=0؛ وگرنه `min(ceil(p/4), max(4, ceil(p/16)))`. یعنی تا p=64 همان `min(ceil(p/4),4)`. مثال‌ها: ۱→۱، ۴→۱، ۵→۲، ۹→۳، ۱۳→۴، ۶۴→۴، ۶۵→۵، ۱۰۰→۷، ۱۶۰→۱۰، ۳۰۰→۱۹.

### ۵.۳ مراحل تصمیم (اولین مرحله‌ای که return کند برنده است)

| مرحله | شرط | اثر | فاز / note |
|---|---|---|---|
| **۰ CONFIRM** (`:470-500`) | `confirm.active`؛ اگر `T≠c.to` یا پروب در جریان ⇒ لغو | G را جمع می‌زند. در پایان پنجره (`confirmWin`=۶۰s): `need = max(c.need, gb + 2·√(varB/nb + varC/n))`. میانگین < need ⇒ `T = from`، `k = kPrev+1`، ثبت `fail`، عقب‌نشینی؛ وگرنه `k=0` | `holding` / `pattern X → Y links: the gain after the last probe did not last (…) — the path is full; next check in D` |
| **۱ FLOOR** (`:503-514`) | `floor = clamp(fGrow)`؛ در `!growable` سقف `S+R`؛ `floor > T` | `pr = nil` (لغو پروب)، `T = floor`، `lastGrowAt = now` — **پرش مستقیم بی‌پروب** | `scaling` / `pattern X → Y links: N active connections (per_link P)` |
| **۲ پروب در جریان** (`:517-519`) | `pr != nil` | `judge` (۵.۴) | `probing` |
| **۳ RESTORE** (`:522-543`) | `isShort && shrinkFrom > T && now−lastShrinkAt ≤ 60s` | `T = shrinkFrom`؛ `undos` در پنجرهٔ ۲h؛ `ttl = 10m << (len(undos)−1)` با سقف ۲h؛ `hold = {T, gPeak, now+ttl}`؛ `lastGrowAt = now` | `scaling` / `… the shrink to X left links at their limit — undone, held for TTL` |
| **۴ GROW** (`:548-579`) | همه با هم: `isShort && shortTick && growable && S ≥ T && T < U && T < max && now ≥ next && len(hist) ≥ 10` | `step = min(ceil(T/4), 32)`؛ اگر `chain>0` و `now−lastGrowAt ≤ 60s` ⇒ `step = min(ceil(T/2), 64)`؛ `to = min(T+step, U, max)`؛ پایه = میانگین و واریانس G در ۱۰ تیک آخر؛ `before[id] = rate10` لینک‌های retiring؛ `T = to` | `probing` / `pattern X → Y links (probe): P of S serving links at their limit, …` |
| **۵ SHRINK** (`:582-603`) | `H < T` به مدت ≥ `shrinkDwell` (۶۰s، از `belowSince`) **و** (`!shortIn60 \|\| T > U`) **و** ≥۳۰s از آخرین shrink **و** ≥۶۰s از آخرین grow | `step = max(1, ceil((T−H)/2))`؛ `T = max(T−step, H)`؛ `shrinkFrom = old` | `shrinking` / `pattern X → Y links: demand needs ~H — …; extra links take no new connections and close when theirs end` |
| **۶ HOLD/STEADY** (`:606-614`) | `isShort && now < next` ⇒ holding؛ وگرنه steady | — | بدون note |

**`out`** (`:834-841`): `T = clamp(T)`؛ اگر `!growable && T > S+R && S+R ≥ min` ⇒ `T = S+R`.

### ۵.۴ چرخهٔ عمر پروب (`judge`، `engine/autopilot.go:618-780`)

فرض کنید تیک N تصمیم GROW گرفت. همان تیک `reconcile` اول retiringها را برمی‌گرداند (بیشترین `open`، بعد تازه‌ترین `lastByte`؛ `engine/linkmanager.go:781-796`) و کسری را dial می‌کند (حداکثر `ceil(T/4)` در تیک، از دروازه؛ `:842-861`). در معکوس، خروجی هدف تازه را از `kindPool` می‌گیرد.

| تیک | رویداد | path:line |
|---|---|---|
| N+1… | **انتظار مسلح‌شدن:** `S ≥ to` ⇒ `armed=true`، `aborts=0`، و همین تیک فقط «waiting» برمی‌گرداند. اگر `now−start > 15s + (to−from)×150ms` ⇒ **abort**: `T=from`، `chain=0`، `aborts++`، `next = now + 60s << (aborts−1)` با سقف ۸m و **بی‌لرزش**؛ `k` عوض نمی‌شود | `:622-645` |
| A+1، A+2 | **نشست** (`settleTicks`=۲) | `:646-649` |
| A+3… | **اندازه‌گیری** هر تیک: `newSum = Σ(rate − before[id])` و `probeSum = Σ rate` روی serving با `servingSince ≥ start`؛ `evalG += G`؛ «کوتاهی» فقط روی لینک‌های فعال (`rate ≥ activeRate`=8KiB/s): `pAct ≥ 1 && act−pAct < spare(pAct)` ⇒ `evalShort++` | `:650-676` |
| n ∈ {5,10,15} = A+7، A+12، A+17 | **نگاه‌ها**، یعنی **۱۴، ۲۴ و ۳۴ ثانیه پس از مسلح‌شدن** | `:677-687` |

آمار هر نگاه (`:688-707`): `rNew = mean(evalNew)`؛ `pTot = mean(evalProbe)`؛ `relieved = 2·evalShort < n`؛ `dG = mean(evalG) − gb`؛ `se = √(varB/nb + varA/n)`؛ `rMin = max(32KiB/s, 0.25·gb/from)`؛ `busy = pTot ≥ rMin`؛ `need = max(0.5·rNew, 2.5·se, 0.05·gb)`.

| حکم (به همین ترتیب، `:708-778`) | شرط | اثر | note |
|---|---|---|---|
| **موفق** (افزایشی) | در هر نگاه: `rNew ≥ rMin && dG ≥ need` | `chain++`، `next = now+4s`، `lastGrowAt=now`، `startConfirm` (اگر شروع نشد ⇒ `k=0`) | `pattern F → T links kept: +X Mbit/s (new links carried Y)` |
| **relieved** | `rNew ≥ rMin && relieved && n==15 && dG > 0 && dG ≥ 0.05·gb` | `chain=0`، `next=+30s`، `lastGrowAt=now`، `startConfirm` | `… kept as headroom: …` |
| **شکست** (جانشینی، «مسیر پر») | `((rNew ≥ rMin \|\| busy) && n==15)` **یا** زودهنگام `(!relieved && rNew ≥ rMin && n==10 && dG < 0.25·rNew)` | `chain=0`، `T=from`، `k++`، `fail={now, max(gb,gPeak), fl60}`، `next = now+backoff()`؛ منطق سقف مسیر | `sized to F links at ~X Mbit/s — N more links carried Y Mbit/s but the total rose only Z (path is full); next check in D` |
| **بی‌نتیجه** | n==15 و هیچ‌کدام | `chain=0`، `next=+30s`، **T در `to` می‌ماند** (spare)، `lastGrowAt` عوض **نمی‌شود** | `… kept as spares: no new connection reached them yet (connections stay on their link)` |

- **سقف مسیر `apCeil`** در شکست (`:755-767`): اگر سقف معتبر باشد (`now−c.at < 1h`) و `from > c.n` و `gb ≤ 1.1·c.g` ⇒ **`T = c.n`** (برگشت به اندازهٔ کوچک‌تری که همین throughput را داشت). اگر سقف نیست، منقضی است، `gb > 1.1·c.g` یا `from < c.n` ⇒ سقف تازه `{from, gb, now}`. وگرنه فقط `c.at = now`.
- **`startConfirm`** (`:800-811`): فقط اگر `k>0` یا شکست کمتر از ۳۰ دقیقه پیش. `need = gb + max(0.5·dG, 0.05·gb)`، `until = now+60s`، `next = until`، `chain = 0`. پس از آنکه مسیر یک‌بار پر دیده شد، هر سود تازه باید یک دقیقه دوام بیاورد؛ در این مدت پروب تازه نیست و زنجیره خاموش است.
- **عقب‌نشینی شکست** (`backoff`، `:784-791`): `30s << (k−1)` با سقف ۸m و ضریب `[0.8, 1.2]` ⇒ ۳۰s، ۱m، ۲m، ۴m، ۸m، ۸m….
- **hold پس از RESTORE:** ۱۰m، ۲۰m، ۴۰m، ۸۰m، ۲h (سقف)؛ باطل اگر `gPeak < 0.6·hold.g`.

### ۵.۵ جدول کامل tunableها (`defaultTunables`، `engine/autopilot.go:126-167` و `:301-307`)

هیچ‌کدام از پیکربندی یا محیط خوانده نمی‌شود.

| نام | مقدار | path:line | معنی |
|---|---|---|---|
| `tick` | 2s (`healthTick`) | `:128` | دورهٔ تصمیم |
| `histTicks` | 30 (=۶۰s) | `:129` | پنجرهٔ `fl60, p60, gPeak, shortIn60` |
| `shortWin` / `shortN` | 5 / 3 | `:130-131` | کمبود = short در ≥۳ از ۵ |
| `armTimeout` + `armPerLink` | 15s + 150ms×لینک | `:132`، `:306`، `armTimeoutFor` `:310-312` | مهلت بالا آمدن لینک‌های پروب |
| `settleTicks` | 2 | `:133` | نشست پس از مسلح‌شدن |
| `looks` | {5,10,15} | `:134` | تیک‌های حکم |
| `baseTicks` | 10 (=۲۰s) | `:135` | پایهٔ پروب و حداقل تاریخچه برای رشد |
| `z` | 2.5 | `:136` | حاشیهٔ نویز |
| `additivity` | 0.5 | `:137` | کل باید ≥ نصف ترافیک لینک‌های تازه بالا برود |
| `minGain` | 0.05 | `:138` | … و ≥۵٪ پایه |
| `earlyFail` | 0.25 | `:139` | شکست زودهنگام در نگاه دوم |
| `rMinAbs` / `rMinFrac` | 32KiB/s / 0.25 | `:140-141` | حداقل ترافیک لینک‌های تازه برای حکم |
| `backoffBase` / `backoffMax` / `jitter` | 30s / 8m / 0.2 | `:142-144` | عقب‌نشینی شکست |
| `successNext` / `inconclusiveNext` / `abortNext` | 4s / 30s / 60s | `:145-147` | فاصلهٔ پروب بعدی |
| `capWindow` / `capMinSamples` / `capMax` | 30m / 6 / `max(256, 4·max)` | `:148-150`، `:272-274` | تخمین ظرفیت |
| `util` / `minBWForNeed` | 0.7 / 16KiB/s | `:151-152` | `needBW` |
| `shrinkDwell` / `shrinkStep` / `noShrinkAfterGrow` | 60s / 30s / 60s | `:153-155` | کوچک‌سازی |
| `overshootWin` | 60s | `:156` | پنجرهٔ RESTORE |
| `holdBase` / `holdMax` / `holdVoid` / `undoWindow` | 10m / 2h / 0.6 / 2h | `:157-160` | hold |
| `kResetAfter` | 30m | `:161` | بازنشانی `k` |
| `chainWindow` | 60s | `:162` | گام ۵۰٪ |
| `activeRate` | 8KiB/s (`pressMinBytes/2s`) | `:163` | لینک زیر آن برای کوتاهی داوری نمی‌شود |
| `confirmWin` / `ceilTTL` | 60s / 1h | `:164-165` | ضد خزش |
| `probeStepMax` / `probeChainStepMax` | 32 / 64 | `:302-303` | سقف گام |

**تخمین ظرفیت** (`noteCap`/`capEstimate`، `:850-890`): برای هر لینک serving+pressed، **بهترین** `sustained` در پنجرهٔ ۳۰ دقیقه نگه داشته می‌شود؛ تخمین = میانهٔ این بهترین‌ها، به شرط ≥۶ لینک متمایز. فقط روی H (کوچک‌سازی) اثر دارد و هرگز خودش T را بالا نمی‌برد.

### ۵.۶ زمان‌ها و مثال‌های عددی (محاسبه از کد؛ سنجیده نشده مگر گفته شود)

- **FLOOR:** جریان سنگین در اولین نمونه flowing می‌شود (EWMA پس از یک تیک ≈۰٫۱۸ نرخ)؛ جریان سبک با قاعدهٔ steady پس از ۳ نمونه (۴–۶s). `fl5` باید ۵ تیک پیاپی بالا بماند (+۸s). پس **≈۸ تا ۱۶ ثانیه**، سپس dial با آهنگ دروازه (~۱۰ در ثانیه). مثال: ۴۰ جریان flowing با `per_link=8` ⇒ `floor = 5`.
- **GROW:** فشار ۲ از ۳ (۲–۶s) + `isShort` ۳ از ۵ (+۴s) ⇒ شروع پروب **≈۶–۱۰s پس از شروع فشار**، به شرط تاریخچهٔ ≥۲۰s و `next`. زودترین حکم موفق ۱۴s پس از مسلح‌شدن ⇒ سریع‌ترین موفقیت ≈۱۶s پس از شروع پروب، و پروب بعدی ≥۴s بعد. گام برای T=8: +۲ (زنجیره +۴)؛ T=100: +۲۵ (+۵۰)؛ T=200: +۳۲ (+۶۴).
- **تخمین رشد ۸→۳۰۰ فقط با پروب‌های موفق پیاپی:** ≈۱۰ پروب × ≈۲۰s ⇒ ۳–۴ دقیقه (**نامطمئن**؛ سنجیده نشده). کف اما پرش مستقیم است: در آزمون بار Q6 با ۲۴۰۰ کاربر فعال، پول در ≈۵۷s به ۳۰۰ لینک رسید (`CHANGELOG.md:810`؛ به احتمال زیاد از مسیر کف — **نامطمئن**).
- **SHRINK:** H خودش تا ۶۰s پس از افت تقاضا بالا می‌ماند (`fl60`، `p60`، `gPeak`)، و `shrinkDwell` ۶۰s دیگر ⇒ **گام اول ≈۱۲۰s پس از افت تقاضا**، بعد هر ≥۳۰s. مثال T=16، H=4: ۱۶→۱۰→۷→۵→۴ در چهار گام، یعنی ≈۲۱۰s پس از افت تقاضا (۱۲۰ + ۳×۳۰). بستن فیزیکی لینک‌ها دقیقه‌ها بعد است (۵.۷).
- **abort:** ۶۰s، ۲m، ۴m، ۸m (بی‌لرزش). شکست: ۳۰s، ۱m، ۲m، ۴m، ۸m (±۲۰٪).

### ۵.۷ محرک (`reconcile`) و فاصلهٔ «T» تا «لینک فیزیکی»

- **رشد:** اول retiringها برمی‌گردند (`servingSince = now`؛ در پروب «لینک تازه» حساب می‌شوند و `before` نرخ قبلی‌شان را کم می‌کند)، بعد dial (`engine/linkmanager.go:781-796, 842-861`).
- **کوچک‌سازی:** لینک‌های اضافه retiring می‌شوند به ترتیب کمترین `flowing` ← کمترین `recent` ← کمترین `open` ← کمترین `rate10` (`:797-816`)؛ `pressed=false`.
- **بستن:** `drainTick` فقط retiringی را می‌بندد که `users==0 && Active()==0` (جریان L3 شمرده نمی‌شود)، حداکثر `closesPerTick(R) = min(max(2, ceil(R/32)), 8)` در تیک (`:983-988, 1062-1064`). اتصال‌های بیکار ≥ `drain_idle` (۳۱۰s) و پس از `retireForce` (۲۰ دقیقه) اتصال‌های «غیر flowing» با FIN بسته می‌شوند (`:1082-1117`).
- پس کاهش T تا بستن واقعی لینک می‌تواند چند دقیقه باشد (`TestSimTracksUpAndDown`: ≤۸ لینک ۱۲ دقیقه پس از افت).
- **اثر روی کاربران:** اتصال‌ها سنجاق‌اند؛ رشد فقط به جریان‌های **تازه** کمک می‌کند. `pickKey` بی‌فشار را ترجیح می‌دهد و همین منطق در شبیه‌ساز تکرار شده (`engine/autopilot_sim_test.go:172-202`).

### ۵.۸ آنچه autopilot نمی‌بیند و آنچه «قفل» می‌شود

- **نمی‌بیند:** RTT، loss، `notsent`، `deliveryRate`، `sndbufUs` (دو تای آخر حتی روی سیم فرستاده می‌شوند ولی خوانده نمی‌شوند؛ `engine/stats.go:236`)؛ `managedLink.goodput` و `lossFrac` نوشته و هرگز خوانده نمی‌شوند (`engine/linkmanager.go:1791-1795, 1852`)؛ فشار حافظه و CPU (grep در `autopilot.go`)؛ `flowing` جریان‌های hs0؛ سقف `max_links` خروجی در معکوس (`capNote` فقط نمایشی است، `engine/linkmanager.go:609-621`).
- **«هیچ ورودی‌ای قفل نمی‌شود»** (توضیح کد، `engine/autopilot.go:41-44`) با **سه استثنا** (اصلاح ۹۱ ردیف ۶۴): hold تا ۲h (تا وقتی قله ≥۶۰٪ بماند)، سقف مسیر `apCeil` تا ۱h (T را **پایین** نگه می‌دارد)، و `k` تا ۳۰m. تخمین ظرفیت هم پنجرهٔ ۳۰m دارد ولی فقط H را مقیاس می‌کند.
- رشد به «همهٔ serving بالا» شرط شده (`S ≥ T`، `:548`): هر کسری serving (dial ناموفق، suspect، degraded در حال جایگزینی) پروب را می‌بندد و اگر حین پروب باشد ممکن است abort کند.
- `belowSince` فقط در مرحلهٔ ۵ به‌روز می‌شود و حکم بی‌نتیجه `lastGrowAt` را تازه نمی‌کند (`:582-588, 771-777`)؛ پس پس از پروب بی‌نتیجه، کوچک‌سازی ممکن است زودتر از ۶۰s اسمی برسد (با نیت «spares را قاعدهٔ shrink برمی‌گرداند» سازگار؛ اثر عملی **نامطمئن**).

### ۵.۹ آزمون‌های autopilot (تضمین‌ها)

شبیه‌ساز جریان‌سطح (`engine/autopilot_sim_test.go`) همان کد واقعی autopilot را با اتصال‌های سنجاق‌شده، سقف هر لینک، سقف مسیر با تقسیم max-min، بستن پنل پس از ۳۰۰s بیکاری، تأخیر dial و خروجی معکوس می‌راند. فشار = تقاضا > تخصیص×۱٫۰۲ و ≥ `pressMinBytes`، ۲ از ۳ با یک تیک تأخیر (`:369-371, 445-448`).

| آزمون | تضمین |
|---|---|
| `TestSimProductionReplay` | ترافیک واقعی (۲۵۰ بیکار، ۱۷ فعال، یک ویدئو ۳Mbit/s): T در ۳ دقیقه ≤۵، هرگز >۸، **صفر پروب**، صفر قطع |
| `TestSimProductionReplayFromStuck32` | از ۳۲ پایین می‌آید (≤۶ در ۴ دقیقه) بی‌قطع اتصال فعال |
| `TestSimTracksUpAndDown` | ۶→۲۵→۶ Mbit/s با throttle ۲Mbit/s: ≥۸۲٪ تقاضا (میانگین ≥۸۸٪)، T≥۱۲ در اوج؛ ≤۸ لینک ۱۲ دقیقه بعد؛ بدون kindStats صفر پروب |
| `TestSimPathFullNoRatchet` / `TestSimConstantPressedNoCreep` / `TestSimNoisyFullPathNoDrift` | مسیر پر: صفر موفقیت و ≤۲۰ پروب در ۲h؛ بی‌خزش؛ ±۲۰٪ نویز ۸ ساعت: T≤۱۰ |
| `TestSimNoGrowthUnderBurstyNoise` / `TestSimReconnectStormIgnored` / `TestSimSinglePinnedCappedFlowNoProbe` | نویز انفجاری، ۳۰۰ handshake در ۱۰s و یک جریان capped رشد نمی‌سازند |
| `TestSimInconclusiveKeepsSpare` | پروب بی‌جریان تازه: بی‌نتیجه، T=۴ نگه داشته، تکرار نمی‌شود |
| `TestSimSevereThrottling` | ۴۰۰kbit/s: ≥۶۰ از ۶۴ جریان flowing و T≥۱۶ |
| `TestAutopilotOvershootRestore` / `TestAutopilotNoShrinkRestoreFlap` / `TestSimOscillationBound` | undo فوری با hold ۱۰/۲۰/۴۰m؛ بی‌نوسان؛ موج مربعی ⇒ ≤۶ تغییر و ≤۲ بستن در ۱۰ دقیقه |
| `TestSimPropertyIdleSettlesAtMin` / `TestSimEnvelope` | بیکاری ⇒ دقیقاً `min`؛ تقاضای نامحدود ≤ max |
| `TestSimProbeArmsOnlyWhenLinksUp` | خروجی dial نمی‌کند ⇒ ≥۲ abort، فاصلهٔ ≥۶۰s |
| `TestProbeVerdictTable` / `TestProbeVerdictNoiseFalsePass` | جدول حکم؛ با CV ۰٫۳ قبول نادرست <۳٪ (۱۰٬۰۰۰ دانه). **رد نادرست مستقیماً آزموده نشده** |
| `TestProbeBackoffAndReset` / `TestBackoffNotResetBySpike` / `TestProbeUnretiredBusyLinksWithoutGainFails` / `TestCapEstimateNotDraggedBySlowLinks` | زمان‌بندی، بازنشانی، و ضد خزش |
| `TestAutopilotScaledParamsMatchSmallPools` / `TestSimPressureGrowthReachesHundreds` (غیرکوتاه) | `spare` تا ۶۴ همان فرمول قدیمی؛ ۶۰۰ جریان ⇒ maxT ≥۲۱۰ و ≥۵۵٪ تقاضا (هر دو حالت) |
| `TestAutopilotFloorNotRepeatedWhenNotGrowable` / `TestAutopilotOutageKeepsSize` | بی pool-control حداکثر یک note؛ قطعی ۱۰ دقیقه‌ای T را عوض نمی‌کند |

---

## ۶. تشخیص لینک خراب و بازیابی

### ۶.۱ نردبان تشخیص (از سریع به کند)

| ناظر | چه می‌سنجد | زمان (از شروع خرابی) | اثر | کجا |
|---|---|---|---|---|
| مهلت نوشتن L3 | `WriteRaw` بیش از ۵s (شامل انتظار برای پنجرهٔ همتا) | **≈۵s**، فقط اگر لینک شلوغ است و ترافیک hs0 روی آن هست (سنجیده: دقیقاً ۵s، `drops=0`) | `markDead` **دائمی** کانال L3 | `engine/l3_link.go:237-240`؛ `engine/stream.go:214-218` |
| wedge guard | خوانندهٔ smux پارک ≥۳ نگاه **و** رلهٔ گیر ≥۶s بی Write کامل | **≈۴–۸s** پس از پارک (عملاً ۶–۸s چون پیشرفت با دانه‌بندی ۲s ثبت می‌شود) | RST فقط رله‌های گیر | `engine/wedge.go:136-160` |
| stuck | `ctrlWait ≥ 6s` در ۲ نمونه با شاهد سالم | **۸ تا ۱۳ ثانیه** (نیم‌باز؛ اصلاح ۹۱ ردیف ۴۰) | degraded + stuck ⇒ تخلیه | `engine/linkmanager.go:1874-1901, 1976-1991` |
| loss | >۱۲٪ بازارسال با ≥96KiB در تیک، ۳ نمونه | آپلود **≈۴–۶s**؛ دانلود **≈۹s+** | degraded ⇒ تخلیه | `engine/linkmanager.go:1816-1860`؛ `engine/loss.go:98-225` |
| suspect | ۱۲s بی‌تغییر `rdBytes` | **۱۲–۱۴s پس از آخرین بایت دریافتی**؛ لینک بیکار سیاه‌چاله ≈۴–۱۴s؛ لینکی که هرگز چیزی نگرفته (`rxSeen=false`) **هرگز** | کنار رفتن از لایه‌های ۰ و ۱ (برگشت‌پذیر) | `engine/linkmanager.go:1735-1744` |
| ناظر نشست L3 | `rdCalls` نشست ۱۲s ثابت | **۱۲–۱۴s** (هر سمت مستقل) | `markDead` کانال L3 | `engine/l3_link.go:143-169` |
| `TCP_USER_TIMEOUT` | ۲۰s دادهٔ تأییدنشده | **≈۲۰s** (لینک شلوغ) تا ≈۲۸s (بیکار؛ NOP هر ۴–۸s) + ≤۲s تا `reap` | مرگ نشست | `tlscarrier/tune_linux.go:22`؛ `engine/stream.go:136-184` |
| keepalive smux | ۲۴s بی سرآیند خوانده‌شده | **۲۴–۴۸s**؛ اگر سطل ≤۰ **هرگز** | مرگ نشست | `engine/mtcp_link.go:278-279`؛ `smux@/session.go:399-422` |
| فشار حافظهٔ TCP | `mem ≥ tcp_mem[1]` (خاموش زیر ۰٫۹ آن) | فوری | توقف همهٔ حکم‌ها + guard تهاجمی | `cmd/hs2/status.go:845-853`؛ `engine/mempressure.go:22-36` |

**چه چیزی لینک را می‌کشد و چه چیزی فقط کنار می‌گذارد:** فقط مرگ نشست یا سوکت (TCP، keepalive smux، خطای I/O)، بستن retiring خالی، پایان تخلیهٔ degraded و `closeAll` لینک را می‌کشند. suspect، retiring و pressed برگشت‌پذیرند؛ degraded برگشت‌ناپذیر است و لینک حداکثر ۹۰s بعد بسته می‌شود.

### ۶.۲ جزئیات قاعده‌ها

**stuck** (`engine/linkmanager.go:1864-1991`، ثابت‌ها `engine/stuck.go:61-95`):
- `waits = !degraded && !draining && !wedged && ctrlWait ≥ 6s && moved < activeBytes(96KiB)` ⇒ `waitingN++`؛ اگر `!suspect` ⇒ `stuckStreak++` و در ≥۲ نامزد (`:1874-1890`).
- **شاهد سالم** (`answering`): `moved ≥ 4KiB && !degraded && !draining && !suspect && peerSeen && ctrlWait < 2s && rtt < 2s && ctrlAns ≠ 0`.
- حکم فقط اگر `len(answering) > 0 && !recovering`؛ به ترتیب طولانی‌ترین انتظار؛ رد اگر `lastAns ≤ c.sent` (هیچ رفت‌وبرگشت کاملی پس از پینگ این لینک شروع نشده)، یا `perTick ≥ max(12KiB, میانهٔ answeringMoved/2)`، یا جای `drainHeadroom − stuckNow` پر است ⇒ `degraded = stuck = true`.
- **محاسبهٔ زمان:** نخستین پینگ بی‌پاسخ در `p0 ∈ [0,3)` فرستاده می‌شود؛ `ctrlWait ≥ 6` در `[6,9)`؛ نخستین تیک در `[6,11)`؛ نمونهٔ دوم در **`[8,13)`**.
- **پیش‌نیاز:** شاهد. در پول ۱–۲ لینکه روی یک مسیر، stuck عملاً کار نمی‌کند.

**مسیر کند / پنجرهٔ بهبود** (`engine/linkmanager.go:1906-1967`؛ `engine/stuck.go:97-138`):
- `usual = promptFloor.base()` = کمینهٔ کمینه‌های دقیقه‌ای ۱۰ دقیقهٔ اخیر از میانهٔ پاسخ‌دهنده‌ها (وقتی ≥۳ پاسخ‌دهنده)؛ `inflated = usual>0 && len(answering) ≥ 3 && promptMed > max(500ms, 4·usual)`.
- `slow = press || waitingN ≥ 2 && (waitingN > len(answering) || inflated)` ⇒ `stuckSlowAt = now`، `stuckSlowFor += dt`.
- تا `stuckRecoverFor = min(max(30s, stuckSlowFor), 2m)` پس از آخرین تیک کند، **هیچ حکم loss یا stuck** صادر نمی‌شود (`recovering`).
- پاسخ‌های ۲ تا ۶ ثانیه‌ای «به هیچ سو شمرده نمی‌شوند» (`engine/stuck.go:39-43`).

**loss** (`engine/loss.go:52-225`):
- **آپلود:** فقط اگر `tsOK && dWr ≥ 96KiB`؛ `segs = ΔData_segs_out` (لینوکس ≥۴٫۶) یا `dWr/1400 + dUp`؛ `frac = dUp/segs` (`:98-107`).
- **دانلود:** پنجره از pong قبلی تا pong فعلی؛ پنجرهٔ <۱s باز می‌ماند؛ اگر `dRd < 96KiB × win/2s` ⇒ «بسته ولی آرام»؛ `segs = ΔsegsIn + dR` (`:115-138`).
- streak هر جهت جدا؛ `!calm` ⇒ صفر؛ نمونهٔ آرام streak را تا ۲۰s از آخرین بد نگه می‌دارد (`engine/linkmanager.go:1832-1849`).
- `lossVerdicts` (`engine/loss.go:180-225`): اگر ≥۴ لینک pressed ⇒ `keep = 0.5 × میانهٔ dom آن‌ها`. اگر ≥۴ لینک داوری‌شده و **بیش از نصفشان** پراتلاف ⇒ «مسیر پراتلاف»، هیچ‌کدام تخلیه نمی‌شود (لاگ دقیقه‌ای). وگرنه به ترتیب بیشترین loss، حداکثر `drainHeadroom(max) − degradedNow` نامزدی که نرخ جهتش < `keep` است ⇒ degraded.
- در تیک `recovering` حکمی صادر نمی‌شود (`engine/linkmanager.go:1948-1950`).

**wedge guard** (`engine/wedge.go:45-182`): یک goroutine برای کل پردازه، هر `guardTick`=۲s روی همهٔ نشست‌ها (لبه و خروجی).
- `parked++` اگر خوانندهٔ smux الان در Read نیست **و** کمتر از `starveCalls`=۲۰۴۸ Read از نگاه قبل داشته؛ `parkedAt` برای قاعدهٔ stuck خوانده می‌شود.
- رلهٔ گیر = `wseq` فرد (در Write محلی) و `now − progress ≥ 6s`.
- `wedged = parked ≥ 3`؛ `squeezed = !wedged && memPressure() && len(stuck)>0`؛ در هر دو، رله‌های گیر با `SetLinger(0)` + `Close` (RST) کشته می‌شوند.
- `wedgedEmpty` (پارک ولی رلهٔ گیری نیست) ⇒ فقط لاگ ۱۰ دقیقه‌ای `… (UDP/TUN backlog or a slow panel dial)`.

**پله‌های تخلیهٔ degraded** (`engine/health.go:74-76`؛ `engine/linkmanager.go:1216-1239, 1361-1398`): کاربر صفر ⇒ بستن فوری؛ بیش از `maxDrain`=۴۵s **یا stuck** ⇒ بستن اتصال‌هایی که ≥ `drainStall`=۱۵s بایت نداشته‌اند؛ بیش از `maxDrainActive`=۹۰s ⇒ بستن لینک. در سقف max و کمبود، قدیمی‌ترین drainingهای گذشته از ۴۵s زودتر بسته می‌شوند (`slotsBack`).

### ۶.۳ خط‌های زمانی

فرض پایه: لبهٔ مستقیم، l3mtcp، `max=32` (پس `drainHeadroom=4`)، `per_link=8`، RTT ≈۱۰۰ms، `t=0` لحظهٔ خرابی. زمان‌های بالای ۱۲s حدود ۲s دقت دارند.

#### الف) سیاه‌چالهٔ یک لینک شلوغ L (همهٔ بسته‌ها در هر دو جهت گم می‌شوند)

| زمان | رویداد | path:line |
|---|---|---|
| `0+` | سوکت L تا پر شدن ۳۲KiB notsent و پنجرهٔ ازدحام می‌پذیرد؛ بعد `sendLoop` گیر می‌کند. روی L بیکار فقط NOP به بافر می‌رود | `tlscarrier/tune_linux.go:19`؛ `smux@/session.go:497` |
| `0–5s` | بسته‌های hs0 هش‌شده به L در صف L می‌مانند؛ صف تا ۲۵۶ پر می‌شود و بقیه در ورود دور ریخته **و شمرده** می‌شوند | `engine/l3_link.go:192-199, 352-355` |
| ≈`5s` (اگر L شلوغ و hs0 روی آن) | `WriteRaw` ⇒ `ErrTimeout` ⇒ `markDead`. **نخستین ناظر.** جریان‌های hs0 لبه→خارج L فوراً به لینک‌های دیگر rendezvous می‌شوند. صف L **بی‌شمارش** رها می‌شود (سنجیده) | `engine/l3_link.go:237-240` |
| `3–6s` | پینگ کنترل بی‌پاسخ؛ `ctrlWait` رشد می‌کند | `engine/control.go:102-148` |
| ≈`6s` | `flowing` جریان‌های L صفر می‌شود (`flowRecent`) ⇒ L «سبک‌ترین» دیده می‌شود و **اتصال تازه جذب می‌کند** تا کلاهک انفجار (≥۴ جاگذاری در ۳ نمونه ⇒ مثل pressed). اتصال تازه روی L تا ۳۰s در `OpenStream` گیر می‌کند (SYN پشت `sendLoop`) | `engine/mtcp_link.go:117`؛ `engine/linkmanager.go:1492-1494`؛ `smux@/session.go:145, 526` |
| **`8–13s`** | **حکم stuck** (اگر شاهد و جا باشد) ⇒ `link N stuck: … — draining`؛ همان تیک `heal`: draining و بازگرداندن یک retiring یا dial جایگزین (make-before-break) | `engine/linkmanager.go:1877-1901, 1976-1991, 2089-2131` |
| `12–14s` | **suspect** (بی‌شرط؛ برای لینکی که از قبل stuck شده هم خط لاگ می‌آید) ⇒ `link N: nothing received for 12s — not used for new connections until it answers`؛ `reconcile` اگر `S<T` و `dialRoom>0` dial می‌کند (suspect جزو max است) | `engine/linkmanager.go:1735-1744, 771-779, 842-861, 2196-2210` |
| `12–14s` | ناظر نشست L3 لبه ⇒ `markDead` (اگر هنوز زنده). **خروجی مستقل** با ناظر خودش یا مهلت ۵s خودش L3 را می‌کشد ⇒ جریان‌های hs0 خارج→لبه هم جابه‌جا می‌شوند. ورودی مردهٔ L3 تا ۳۰s در `l3Set` می‌ماند (FIN پشت `sendLoop`)، بی‌ضرر برای `pick` | `engine/l3_link.go:143-161, 183-189` |
| **≈`16–19s`** (فقط مسیر stuck) | `reclaimStalled` هر جریان کاربر بی‌بایت در ۱۵s را می‌بندد؛ `die` فوراً بسته، رله تمام، کاربر FIN می‌گیرد. (`lastActive` با دانه‌بندی تیک ثبت می‌شود و گذر فقط در تیک‌های `heal` است؛ اصلاح ۹۱) | `engine/linkmanager.go:1225-1236, 1361-1398`؛ `engine/mtcp_link.go:111` |
| ≈`20s` (شلوغ) تا ≈`28s` (بیکار) | `TCP_USER_TIMEOUT` لبه ⇒ `watchConn` نشست را می‌بندد ⇒ همهٔ جریان‌های L می‌میرند؛ رله‌ها تا `relayDieGrace`=۵s بافر را تحویل می‌دهند؛ تیک بعد `mtcp: link N down: <reason>` (متن دقیق **نامطمئن**: احتمالاً `read: timed out (path stalled)`) | `engine/stream.go:136-184`؛ `engine/wedge.go:325-341`؛ `engine/linkmanager.go:1408-1434` |
| `20–48s` (خروجی) | `USER_TIMEOUT` خروجی یا keepalive smux ⇒ `link down from …`؛ رله‌های پنل بسته | `engine/stream_kharej.go:144-154` |

- **hs0:** جریان‌های هش‌شده به L در هر جهت جداگانه از `t=0` تا مرگ L3 همان سمت (۵s یا ۱۲–۱۴s) گم می‌شوند و بعد روی لینک دیگری ادامه می‌دهند؛ hs2 اتصال درونی را قطع نمی‌کند (TCP درونی ممکن است با backoff خودش دیرتر راه بیفتد — **نامطمئن**). وقتی جایگزین بالا آمد و `OnLink` آن L3 باز کرد، ≈1/N جریان‌های hs0 هر سمت به آن منتقل می‌شوند (خطر بی‌ترتیبی).
- **کاربران TCP:** روی L در ≈۱۶–۱۹s (مسیر stuck) یا ≈۲۰–۳۳s (`USER_TIMEOUT` + مهلت تحویل) قطع می‌شوند؛ اتصال‌هایی که بین `t=0` و حکم روی L نشسته‌اند همین سرنوشت را دارند.
- **بی‌شاهد** (پول کوچک): فقط suspect و مرگ TCP می‌ماند.

#### ب) همان سیاه‌چاله در حالت معکوس

- لبه dial نمی‌کند (`engine/linkmanager.go:826-828`).
- **اگر stuck حکم شود:** `sweepReverse` لینک را draining می‌کند و `ctlTarget` یکی بالا می‌رود (`:1145-1210, 1340-1349`) ⇒ خروجی یک slot اضافه می‌کند: روی ۲ لینک سریع فوری، روی بقیه ≤۱٫۵s.
- **اگر فقط suspect باشد:** `ctlTarget` فقط draining را می‌شمارد (`:1323-1349`)، پس **جایگزینی خواسته نمی‌شود** تا slot خروجی خودش مرگ L را بفهمد (≈۲۰–۴۸s)؛ سپس پس از `jitterDur(500ms)` (۲۵۰–۵۰۰ms اگر لینک ≥۳۰s زنده بوده) و یک نوبت دروازه دوباره dial می‌کند (`engine/exit_pool.go:363-417`). لینک suspect در `countsLocked` serving نیست، پس جایگزین spare به دنیا نمی‌آید (`engine/linkmanager.go:416-418`).
- لبه L را وقتی نشستش بسته شد با `DropLink` کنار می‌گذارد: `mtcp: reverse link N from IP down: …` (`engine/stream_reverse.go:93-98`؛ `engine/linkmanager.go:474-496`).

#### ج) لینک پراتلاف (>۱۲٪، با ≥96KiB در تیک)

| زمان | رویداد |
|---|---|
| ۳ نمونهٔ بد (آپلود ≈۴–۶s؛ دانلود ≈۳ پنجرهٔ pong ≈۹s+) | streak؛ نمونهٔ آرام آن را تا ۲۰s نگه می‌دارد |
| حکم | اگر calm، اکثریت ≥۴ لینک داوری‌شده پراتلاف نباشند، نرخ لینک < نیمِ میانهٔ pressedها (وقتی ≥۴)، و جا باشد ⇒ `link N degraded (up-loss …, down-loss …, moving X Mbit/s where the busy links get Y, rtt …) — draining` (`engine/loss.go:221-222`) |
| همان تیک | `heal`: draining + جایگزین؛ کاربر تازه نمی‌گیرد |
| +۴۵s | `… degraded for 45s — its connections that moved no data for 15s (idle or stuck) are closed now …` |
| +۹۰s | `… degraded for 1m30s — closed with its N remaining connection(s) …` |

hs0 روی این لینک تا بسته شدنش می‌ماند (`l3Set.pick` فقط `Alive()` کانال L3 را می‌بیند).

#### د) throttle DPI (چند بسته در ثانیه؛ قطع نمی‌شود)

- گاهی قابی می‌رسد ⇒ هرگز suspect نمی‌شود؛ زیر ۹۶KiB در تیک ⇒ loss داوری نمی‌کند. قاعدهٔ stuck دقیقاً برای همین است (`engine/stuck.go:8-15`). حکم ≈۸–۱۵s اگر شاهد باشد. ناظر L3 قاب‌های دریافتی را می‌بیند و L3 را **نمی‌کشد**، مگر یک `WriteRaw` ≥۵s پشت نویسنده بماند.
- **سنجیده (CHANGELOG، Q7):** ۱۰ لینک پرکار گیرکرده ⇒ ۹ از ۱۰ در ۱۰٫۶s؛ پاسخ echo از ۷۶٪ به ۱۰۰٪ در ۶۰s؛ پول کوچک (max 10) با ۴ لینک گیر ⇒ ۹–۱۵s (`CHANGELOG.md:912-950, 1052`).
- اگر **اکثریت** لینک‌ها throttle شوند ⇒ «مسیر کند»، هیچ تخلیه‌ای نیست (عمدی).

#### هـ) قطعی کامل مسیر به مدت D

نخستین ناظرها: نویسنده‌های smux لینک‌های شلوغ گیر می‌کنند (بی‌لاگ) ← مهلت ۵s L3 روی همان‌ها ← لاگ «مسیر کند» (≥۲ لینک منتظر و کسی سریع پاسخ نمی‌دهد: `%d of %d busy links have waited 6s+ … none is drained`؛ از آن پس ۳۰s تا ۲m هیچ حکمی) ← suspect و مرگ L3 در ۱۲–۱۴s ← `USER_TIMEOUT` ≈۲۰s.

| بازهٔ D | لینک‌های mtcp | کاربران TCP | hs0 |
|---|---|---|---|
| کمتر از ≈۵s | زنده می‌مانند؛ توقف = D + backoff بازارسال هسته (**نامطمئن**) | قطع نمی‌شوند | بسته‌های صف که پس از آزاد شدن نویسنده >۶۰ms سن داشته باشند دور ریخته و شمرده می‌شوند. اگر backoff هسته گرفتگی را به ۵s برساند، **L3 لینک‌های شلوغ می‌میرد** (**نامطمئن**) |
| ۵ تا ۱۲s | زنده؛ suspect نمی‌شوند | قطع نمی‌شوند؛ اتصال تازه شاید تا ۳۰s در `OpenStream` بماند | L3 هر لینکی که `WriteRaw` در انتظار داشت ۵s بعد می‌میرد (هر سمت مستقل)؛ L3 لینک‌های بیکار زنده می‌ماند؛ جریان‌ها به لینک‌های دارای L3 می‌روند |
| ۱۲ تا ≈۲۰s | همه suspect (یک لاگ برای هر لینک، تا نمی‌خورد). لایهٔ ۲ `pickLocked` باز هم لینک suspect می‌دهد. `reconcile` چون `S=0` dial صف می‌کند (اگر `dialRoom>0`)؛ اتصال این dialها تا ۸s در سیاه‌چاله می‌ماند؛ ۳ شکست پیاپی ⇒ `epoch` صف را خالی می‌کند. بقای لینک‌های قدیمی به زمان‌بندی هسته در برابر ۲۰s بستگی دارد (**نامطمئن**) | اگر لینک‌ها زنده بمانند، می‌مانند | **L3 همهٔ لینک‌ها در هر دو سمت می‌میرد.** اگر لینک‌ها زنده بمانند suspect با نخستین بایت بی‌لاگ پاک می‌شود، ولی hs0 فقط از لینک‌های **تازه** جان می‌گیرد (`openL3` فقط در `OnLink`). dialهایی که هنگام بازگشت مسیر در جریان‌اند ممکن است لینک تازه با L3 بیاورند؛ ولی تیک بعد serving از T بیشتر می‌شود و `reconcile` اضافه‌ها را retire می‌کند — معمولاً همین لینک‌های تازهٔ بی‌کاربر — و `drainTick` retiring خالی را ۲ تا ۸ در تیک می‌بندد. **پس hs0 ممکن است فقط موقتاً برگردد و دوباره کاملاً قطع شود** تا تغییر بعدی پول؛ تنها نشانه خط ۳۰ ثانیه‌ای `l3: dropped …` است (استنتاج؛ **نامطمئن**) |
| بیش از ≈۲۰s | `USER_TIMEOUT` هر دو سمت (بیکار تا ≈۲۸s؛ keepalive smux ۲۴–۴۸s پشتیبان)؛ `reap` همه (لاگ‌ها با `burstLog` تا می‌خورند)؛ `exitInfo=nil`؛ dial هر تیک شکست؛ هر ۳۰s `mtcp: want %d serving links, only %d up — dials failing (peer down or path blocked): %s` | همه قطع (FIN پس از ≤۵s)؛ کاربر تازه ≈۶s (`pickWait`) صبر و بعد بسته | همه با «no link» دور ریخته و شمرده |

**بازیابی پس از قطعی بلند:** تیک بعد dial موفق ⇒ نخستین لینک ⇒ اپیزود refill (≤۱۰s، سقف = سهم منصفانه `max(per_link, ceil((users+queue)/T))`؛ `engine/refill.go:94-145`) ⇒ hs0 با نخستین لینک برمی‌گردد. `reconcile` هر تیک تا `ceil(T/4)` لینک صف می‌کند ⇒ برای T=8 پول در ≈۸s کامل (استنتاج). **سنجیده:** قطعی ۴۰s ⇒ سرویس عادی ۱۶–۲۰s پس از پایان (قبلاً ۶۱s)؛ ری‌استارت ایران زیر بار ⇒ ۱۶٫۳s (قبلاً ۷۱s) (`CHANGELOG.md:812-813`).

**معکوس:** هر slot خروجی پس از مرگ لینکش دوباره dial می‌کند؛ با نخستین شکست وقتی هیچ لینکی زنده نیست، «outage» شروع می‌شود: فقط یک slot کاوشگر با connect ۲s و backoff ≤۲s تلاش می‌کند و بقیه منتظرند (`engine/exit_pool.go:251-302, 353-356`؛ `cmd/hs2/main.go:534-536`). لاگ‌ها: `mtcp: no link up to the edge — dials fail (%v); one slot keeps trying …` و `mtcp: a link to the edge is back after %s with none up …`.

#### و) ری‌استارت سرور خارج

- **آرام، مستقیم:** خارج `sess.Close()` ⇒ `close_notify` + FIN (استنتاج)؛ فرایند حداکثر ۳s بعد `os.Exit` (`engine/stream_kharej.go:133-139`؛ `cmd/hs2/main.go:380-385`). لبه در ≈۱ RTT EOF می‌خواند ⇒ «closed by the other server» ⇒ **همهٔ** اتصال‌ها و L3ها می‌میرند؛ hs0 با «no link» دور می‌ریزد. ≤۲s بعد `reap` و dial؛ تا خارج گوش ندهد `ECONNREFUSED` فوری و هر تیک تکرار. پس از گوش دادن: ≈۳ RTT تا لینک ۱ ⇒ refill ⇒ hs0 برمی‌گردد؛ ≈۸s تا پول کامل (T=8، استنتاج؛ عدد سنجیده‌ای برای ری‌استارت خارج نیست).
- **hs0 خارج** با بسته شدن fd از بین می‌رود و `OpenWith` آن را حذف و از نو می‌سازد؛ فقط نشانی و route خود hs2 برمی‌گردد؛ **مسیرهای دستی اپراتور روی hs0 خارج از دست می‌روند** (`tun/tun_linux.go:100-102, 122-130`؛ استنتاج از رفتار tun غیرماندگار).
- **آرام، معکوس:** لبه `DropLink` فوری؛ autopilot T را نگه می‌دارد؛ خارج با `WarmSize(min,max)` شروع می‌کند و `openPoolCtl` هدف لبه را می‌فرستد. لینک‌هایی که خارج بیش از هدف dial کند spare به دنیا می‌آیند ولی **باز هم L3 دارند و جریان hs0 می‌گیرند**؛ پس از `bornSpareGrace`=۳۰s اگر خالی باشند بسته می‌شوند ⇒ جریان‌های hs0 دوباره جابه‌جا می‌شوند.
- **کرش** (`kill -9`، OOM): مثل آرام، بی `close_notify`؛ دلیل «closed by the other server» یا «reset by the network or the other server».
- **ری‌استارت میزبان / قطع شبکهٔ خارج:** مثل (هـ) بلند. در معکوس سقف پذیرش `2×max+8` لینک‌های **زنده** را می‌شمارد و لینک‌های قدیمی تا کشف مرگ زنده‌اند (`engine/stream_reverse.go:26-41, 68`).

#### ز) کوچک‌سازی عادی (خرابی نیست)

retiring ⇒ کاربر تازه نمی‌گیرد؛ بستن وقتی `users==0 && Active()==0`؛ بیکار ≥۳۱۰s و پس از ۲۰m غیر flowingها بسته می‌شوند؛ **جریان‌های TUN روی آن هنگام بستن جابه‌جا می‌شوند** (جریان L3 لینک را نگه نمی‌دارد).

### ۶.۴ آنچه هیچ ناظری نمی‌گیرد

- لینکی که پس از احراز **هیچ** قاب smuxی نمی‌گیرد: suspect هرگز (`rxSeen=false`، `engine/linkmanager.go:1736-1740`)؛ فقط stuck (با شاهد) یا `USER_TIMEOUT`.
- خوانندهٔ کند ولی زنده‌ای که سطل نشست را خالی نگه می‌دارد؛ لینک پراتلاف کم‌حجم؛ تأخیر ۲ تا ۵ ثانیه‌ای؛ اشباع CPU (جزئیات در §۷.۴).
- کانال L3 که روی لینک زنده بمیرد **هرگز** دوباره باز نمی‌شود (`kindL3` فقط در `engine/stream_iran.go:127, 428` و `engine/stream_kharej.go:245`)؛ کانال کنترل هم پس از خطای غیر timeout دوباره باز نمی‌شود (`engine/control.go:142-143, 156-157`)؛ در آن حالت لینک تا پایان عمر نه loss دانلود دارد نه stuck. در مقابل، کانال آمار هر ۵s دوباره باز می‌شود (`engine/stats.go:97-121`) و `kindInfo` هر دقیقه دوباره تلاش می‌کند (`engine/stream_iran.go:104-122`).

---

## ۷. همهٔ حلقه‌های کنترلی، ماتریس تعامل و شکاف‌ها

### ۷.۱ ساعت‌ها

**ساعت مشترکی وجود ندارد.** در l3mtcp دست‌کم هفت ساعت مستقل و بی‌هم‌فاز کار می‌کنند:

| ساعت | دوره | سمت | path:line |
|---|---|---|---|
| تیک پول (`Run` / `runAccept`) | 2s | فقط لبه | `engine/health.go:18`؛ `engine/linkmanager.go:567-581, 1125-1139` |
| نگهبان wedge (یکی برای کل پردازه) | 2s | هر دو | `engine/wedge.go:62, 220-226` |
| نویسندهٔ وضعیت (`tcpMemWatch`، `cpuMeter`، `hostMeter`، warm) | 2s | هر دو | `cmd/hs2/status.go:29, 335-347` |
| کانال کنترل هر لینک | ≥96KiB در تیک ⇒ 3s؛ ≥4KiB ⇒ 2–4s با لرزش؛ بیکار ⇒ هر ۳ تا ۵ تیک (۹–۱۵s)؛ ping معلق پرش بیکار را لغو می‌کند؛ روی لینک مرده عملاً ۶–۹s | لبه می‌پرسد، خروجی پاسخ می‌دهد | `engine/control.go:102-148` |
| keepalive smux هر نشست | NOP تصادفی ۴–۸s، بررسی ۲۴s | هر دو | `engine/mtcp_link.go:278-279` |
| ناظر نشست کانال L3 | 2s (سکوت ۱۲s) | هر دو | `engine/l3_link.go:143-161` |
| refill | 100ms (≤۱۰s) | لبه | `engine/refill.go:56` |

**نکتهٔ ترتیب:** حکم‌های loss و stuck که در `sampleHealth` صادر می‌شوند در **همان تیک** به `heal` (مستقیم) یا `sweepReverse` (معکوس) می‌رسند؛ سپس autopilot با `apSample` همان تیک تصمیم می‌گیرد. لینکی که در این تیک degraded شده در نمونه دیگر serving نیست، ولی جایگزینی که `heal` تازه صف کرده هنوز در نمونه نیست (`engine/linkmanager.go:2066-2073`). در معکوس، `reap` و `heal` صدا زده نمی‌شوند؛ کارشان را `sweepReverse` (`:1145-1210`) و `DropLink` (`:474-496`) می‌کنند.

**همهٔ تصمیم‌های سطح پول فقط در لبه گرفته می‌شوند.** خروجی حلقهٔ سلامت لینک ندارد؛ فقط wedge guard، keepalive smux، `TCP_USER_TIMEOUT`، ناظر L3، و در معکوس پول slot که از لبه فرمان می‌گیرد (`engine/exit_pool.go:14-30`).

### ۷.۲ کاتالوگ حلقه‌ها (`L01`–`L41`، مسیر mtcp / l3mtcp)

| ID | حلقه | دوره | واکنش (محاسبه، مگر سنجیده گفته شود) | محل |
|---|---|---|---|---|
| `L01` | تیک پول (ارکستراتور)؛ پرشدن اولیه `min(warm, ceil(warm/4)+8)` | 2s | — | `engine/linkmanager.go:546-583, 564-566, 1124-1140` |
| `L02` | FLOOR | 2s | ≈۸–۱۶s | `engine/autopilot.go:503-514` |
| `L03` | GROW و داوری پروب | 2s | شروع ≈۶–۱۰s پس از فشار؛ حکم ۱۴–۳۴s پس از مسلح‌شدن | `engine/autopilot.go:548-579, 618-780` |
| `L04` | CONFIRM و سقف مسیر | 2s | پنجرهٔ ۶۰s؛ سقف ۱h | `engine/autopilot.go:470-500, 755-767, 800-811` |
| `L05` | SHRINK | 2s | گام اول ≈۱۲۰s پس از افت؛ سپس ≥۳۰s | `engine/autopilot.go:582-603` |
| `L06` | RESTORE و hold | 2s | همان تیک؛ hold ۱۰m→۲h | `engine/autopilot.go:522-543` |
| `L07` | backoff، abort، بازنشانی k | 2s | ۳۰s تا ۸m | `engine/autopilot.go:448-461, 626-643, 784-791` |
| `L08` | تخمین ظرفیت هر لینک | 2s | پنجرهٔ ۳۰m | `engine/autopilot.go:850-890` |
| `L09` | `reconcile` (محرک) | 2s | همان تیک | `engine/linkmanager.go:767-862` |
| `L10` | `drainTick` (بستن retiring و بازپس‌گیری) | 2s | ۲–۸ در تیک؛ بیکار ۳۱۰s؛ اجبار ۲۰m | `engine/linkmanager.go:959-1117` |
| `L11` | فایل warm | نوشتن 2s (پس از ۱m)؛ خواندن یک‌باره | ≤۱۵m عمر | `cmd/hs2/status.go:192-246`؛ `cmd/hs2/main.go:280-293` |
| `L12` | `Pick`/`pickKey` و کلاهک انفجار | هر اتصال | فوری | `engine/linkmanager.go:1470-1586` |
| `L13` | فشار آپلود | 2s | ۲–۶s | `engine/linkmanager.go:1797-1806` |
| `L14` | فشار دانلود (`kindStats`) | با poll | ≈۴–۶s؛ کهنگی ۶s | `engine/linkmanager.go:1801-1814, 2032-2064`؛ `engine/stats.go:106-249` |
| `L15` | refill hold | 100ms | ≤۱۰s | `engine/refill.go:94-351` |
| `L16` | دروازهٔ info | یک‌باره | ≤۵s؛ تا ~۳۵s اگر نویسنده گیر باشد | `engine/peerinfo.go:267-307`؛ `engine/linkmanager.go:1554` |
| `L17` | suspect | 2s | ۱۲–۱۴s پس از آخرین بایت | `engine/linkmanager.go:317, 1735-1744` |
| `L18` | کانال کنترل ping/pong | ۳s / ۲–۴s / ۹–۱۵s | — | `engine/control.go:72-181` |
| `L19` | stuck | 2s | ۸–۱۳s | `engine/linkmanager.go:1864-1901, 1968-1991` |
| `L20` | مسیر کند/شلوغ و پنجرهٔ بهبود | 2s | فوری؛ مهار ۳۰s–۲m | `engine/linkmanager.go:1906-1950`؛ `engine/stuck.go:97-138` |
| `L21` | loss | 2s / پنجرهٔ pong | ≈۴–۶s (آپلود)؛ ≈۹s+ (دانلود) | `engine/linkmanager.go:1816-1860`؛ `engine/loss.go:98-225` |
| `L22` | تخلیهٔ degraded (`heal`/`sweepReverse`) | 2s | همان تیک؛ پله‌های ۴۵/۱۵/۹۰s | `engine/linkmanager.go:2089-2182, 1145-1291, 1361-1398` |
| `L23` | `reap`/`DropLink` و هشدار لینک کوتاه‌عمر | 2s / رخداد | ≤۲s / فوری | `engine/linkmanager.go:1408-1462, 474-496` |
| `L24` | صف dial و دروازه (مستقیم) | رخداد | ≥۴۰ms بین شروع‌ها؛ connect ≤۸s؛ auth ≤۱۰s | `engine/linkmanager.go:891-950`؛ `engine/dialgate.go:22-75` |
| `L25` | slotهای پول خروجی (معکوس) | رخداد | backoff ۰٫۵–۸s؛ scout ≤۲s | `engine/exit_pool.go:41-432` |
| `L26` | pool-control و نگهبان‌های churn (معکوس) | ۲٫۴–۳٫۶s / ۲۴–۳۶s / تغییر | ≤۱٫۵s برای تغییر | `engine/exit_pool.go:448-584`؛ `engine/linkmanager.go:409-470, 959-1018`؛ `engine/stream_reverse.go:36-101` |
| `L27` | wedge guard | 2s | ≈۴–۸s پس از پارک | `engine/wedge.go:45-280` |
| `L28` | فشار حافظهٔ TCP هسته | 2s | فوری؛ اثر تا ۳۰s–۲m بعد | `cmd/hs2/status.go:831-855`؛ `engine/mempressure.go:22-36` |
| `L29` | `relayDieGrace` | رخداد | ۵s | `engine/wedge.go:68-70, 328-339` |
| `L30` | keepalive smux | NOP ۴–۸s؛ بررسی ۲۴s | ۲۴–۴۸s | `engine/mtcp_link.go:278-279`؛ `smux@/session.go:399-422` |
| `L31` | `watchConn` و `TCP_USER_TIMEOUT` | پیوسته | ۲۰s (+≤۲s تا reap) | `engine/stream.go:74-123`؛ `tlscarrier/tune_linux.go:22, 46` |
| `L32` | پس‌فشار smux و `TCP_NOTSENT_LOWAT` | پیوسته | — | `engine/mtcp_link.go:258-264`؛ `tlscarrier/tune_linux.go:19, 44` |
| `L33` | مهلت‌های SYN/FIN smux و دست‌دهی TLS | رخداد | ۳۰s / ۱۰s / ۳۰s | `smux@/session.go:17, 524-527`؛ `tlscarrier/server.go:40, 47` |
| `L34` | کانال L3 (صف، کهنگی، keepalive، ناظر نشست) | هر بسته / 2s | ۶۰ms / ۵s / ۱۲–۱۴s / ۳۰s | `engine/l3_link.go:49-393` |
| `L35` | جریان‌های UDP کاربر | هر datagram / ۳۰s | ۲–۲٫۵m بیکاری | `engine/stream_iran.go:270-416`؛ `engine/stream.go:270-300` |
| `L36` | مهلت‌های خروجی (نوع جریان، dial پنل) | رخداد | ۱۰s / ۵s | `engine/stream_kharej.go:173-244` |
| `L37` | `setMemoryLimit` (نصف RAM یا `GOMEMLIMIT`) | یک‌باره | — | `cmd/hs2/main.go:305-320` |
| `L38` | tune (sysctl) | یک‌باره در شروع | — | `cmd/hs2/main.go:256-275, 351-359` |
| `L39` | `cpuMeter`/`hostMeter` | 2s | ۳ / ۵ نمونه؛ در l3mtcp **فقط لاگ** | `cmd/hs2/status.go:351-404`؛ `cmd/hs2/hostcpu.go:58-242` |
| `L40` | `acceptBackoff` | رخداد | ۵ms تا ۱s | `engine/engine.go:211-239` |
| `L41` | `burstLog` و محدودکننده‌های لاگ | ۱۰s / ۱m / ۱۰m | — | `engine/burstlog.go:17-73` |

### ۷.۳ ماتریس تعامل (فشرده)

**نوع:** هماهنگ (طراحی‌شده)، خنثی‌کننده (یکی دیگری را می‌بندد)، رقیب (هر دو روی یک چیز)، کور (بی‌خبر از هم). قطعیت فقط دربارهٔ وجود سازوکار است؛ اثر عملی جدا علامت خورده.

| # | حلقه‌ها | نوع | سازوکار (path:line) | اثر |
|---|---|---|---|---|
| 1 | `L17`/`L19`/`L21` ← `L03` | خنثی‌کننده | suspect و degraded در `serving` نیستند؛ GROW به `S ≥ T` و مسلح‌شدن به `S ≥ to` نیاز دارد (`engine/autopilot.go:548, 623`) | رشد تا آمدن جایگزین بسته است؛ حین پروب ⇒ abort با ۶۰s تا ۸m. اثر عملی **نامطمئن** |
| 2 | `L17` ← `L09` | خنثی‌کننده | `dialRoomLocked` suspect را slot می‌شمارد (`engine/linkmanager.go:2196-2210`) | در سقف max، suspect جایگزین نمی‌گیرد تا TCP آن را بکشد (≈۲۰–۳۰s) |
| 3 | `L17` ← `L12` لایهٔ ۲ و `L15` | کور | لایهٔ ۲ suspect را می‌پذیرد (`:1560-1570`)؛ refill فقط با `alive==0` (`engine/refill.go:100`) | در قطعی کامل، کاربر تازه روی لینک مرده جا می‌گیرد |
| 4 | `L12` در برابر لینک تازه‌مرده | رقیب | `flowing` با `flowRecent=6s` صفر می‌شود؛ کلید کمترین `flowing+picks` (`engine/mtcp_link.go:117`؛ `engine/linkmanager.go:1480-1482`) | از ≈۶s تا stuck/suspect (۸–۱۴s) لینک خراب «سبک‌ترین» است؛ فقط کلاهک انفجار مهار می‌کند |
| 5 | استثنای rwnd در `L13`/`L14` ← `L12`/`L03` | کور | `rwnd < 0.5` شرط فشار است (`:1798, 2057`) | لینکی که گیرنده‌اش کند است بی‌فشار دیده می‌شود: pick **ترجیحش** می‌دهد و autopilot برایش رشد نمی‌کند |
| 6 | `L27` ← `L19` | خنثی‌کننده | `waits` به `!wedged` نیاز دارد (`:1877`) | اگر نگهبان نتواند آزاد کند (`wedgedEmpty`)، لینک **دائماً** از stuck معاف است |
| 7 | `L27` ← `L30` | بی‌پشتیبان | keepalive با سطل ≤۰ نشست را نمی‌بندد | نشست پیوسته wedge با keepalive نمی‌میرد |
| 8 | `L27` ← `L34` | رقیب | ناظر L3 روی `rdCalls`؛ L3 فقط در `OnLink` | wedge ≥۱۲s کانال L3 آن لینک را **برای همیشه** حذف می‌کند |
| 9 | `L28` ← `L19`/`L21`/`L27` | هماهنگ | `calm`/`slow`/`recovering`؛ squeezed (`engine/wedge.go:158`) | زیر فشار حافظه کسی محکوم نمی‌شود و رله‌های گیر آزاد می‌شوند |
| 10 | `L28` در برابر خوانندهٔ کند ولی زنده | شکاف | نگهبان فقط رلهٔ «گیر» (۶s بی Write کامل) را می‌کشد (`engine/wedge.go:153`) | فشار می‌ماند و **همهٔ حکم‌ها تا پایان آن + ۳۰s–۲m خاموش‌اند** (**نامطمئن**) |
| 11 | `L21` و `L19` | رقیب | سقف‌های جدا: `drainHeadroom − degradedNow` (`engine/loss.go:209`) و `drainHeadroom − stuckNow` (`engine/linkmanager.go:1981`) | در یک تیک تا ≈۲× headroom تخلیه ممکن است؛ قصد طراحی **نامطمئن** |
| 12 | `L21` ← `L13`/`L14` | هماهنگ | `keep = 0.5 × میانهٔ pressedها` فقط با ≥۴ pressed | با کمتر، لینک پراتلاف تنها داوری می‌شود |
| 13 | `L20` ← `L21`/`L19` | هماهنگ | `recovering` | در طلسم کندی هیچ حکمی نیست |
| 14 | `L22` (`heal`) ← `L09` | هماهنگ با نقص | `heal` retiring را بی‌سنجش suspect برمی‌گرداند (`:2103-2108`) | un-retire بی‌اثر؛ `reconcile` همان تیک باز dial می‌کند |
| 15 | `L22`/`L09`/`L24` (epoch) | هماهنگ | ۳ شکست ⇒ `epoch++` (`:921-923`) | جایگزین‌های صف‌شده هم رها می‌شوند؛ تیک بعد دوباره صف |
| 16 | `L10` ← `L34` | کور | جریان L3 خام است و `Active()` آن را نمی‌شمارد (`:983`) | کوچک‌سازی و بستن جریان‌های TUN را جابه‌جا می‌کند؛ هر لینک تازه ≈1/(n+1) را |
| 17 | `L34` در برابر `L17`/`L19`/`L21`/`L10` | کور | `l3Set.pick` فقط `Alive()` کانال L3 (`engine/l3_link.go:315`) | TUN روی degraded تا ۹۰s؛ روی suspect/stuck تا ۱۲–۱۴s یا مرگ لینک |
| 18 | `L02` در برابر بار TUN | کور | جریان L3 در `flowStats` نیست | FLOOR بار hs0 را نمی‌بیند؛ رشد فقط از پروب |
| 19 | churn guard، `retireAfterDrop`، born spare ← `L10`/`L25` | هماهنگ | `engine/linkmanager.go:442-470, 963-985` | جلوگیری از چرخهٔ «لبه می‌بندد، خروجی dial می‌کند» |
| 20 | `L25` (`incLive`) در برابر هدف لبه | هماهنگ | `engine/exit_pool.go:424-430`؛ refill «stalled» در ۳s | پس از قطعی خروجی با warm بالا می‌آید و لبه هدف را دوباره می‌فرستد |
| 21 | `L15` در برابر `L02` | هماهنگ | سقف refill = `ceil(D/T)` | FLOOR در اپیزود سقف هر لینک را پایین می‌آورد |
| 22 | `L14` (poll فقط پرکار) ← `L28` طرف مقابل | کور | poll فقط ≥16KiB/تیک | فشار حافظهٔ طرف مقابل روی پول بیکار دیده نمی‌شود (**نامطمئن**، احتمالاً بی‌اهمیت) |
| 23 | `L28`/`L39` ← `L03` | کور | autopilot نه حافظه می‌خواند نه CPU | زیر فشار CPU پروب‌ها ادامه می‌یابند؛ شرط افزایشی احتمالاً ردشان می‌کند (**نامطمئن**) |
| 24 | `L32` ← `L13` | وابستگی ایستا | `engine/health.go:107-113`؛ MPTCP به همین دلیل خاموش | تغییر `HS2_TUNE_NOTSENT` آستانهٔ فشار را بی‌صدا جابه‌جا می‌کند |
| 25 | `L31`/`L30`/`L17`/`L26` | هماهنگ | `suspectAfter 12s < USER_TIMEOUT 20s < keepalive 24s < bornSpareGrace 30s` (`engine/linkmanager.go:303-317`) | جایگزین معکوس پیش از تشخیص مرگ قدیمی بسته نمی‌شود |
| 26 | `reclaimStalled` در برابر FIN ۳۰s | هماهنگ | بستن موازی (`:1351-1360, 1378-1382`) | بستن‌ها پشت هم ۳۰s معطل نمی‌شوند |
| 27 | `L18` در برابر `L32` | هماهنگ (طراحی حسگر) | ping پشت صف خود لینک است (`engine/control.go:17-28`) | `ctrlWait` همان انتظار کاربران است |
| 28 | `L12`/`L16` در برابر `L33` | کور | SYN با ۳۰s؛ سرآیند بی‌مهلت (`engine/stream_iran.go:248-252`) | کاربر روی لینک گیرِ بی‌علامت تا ۳۰s برای هر تلاش یا تا مرگ لینک معطل |
| 29 | `L39` ← `L13` | کور | `blockedMin = 1ms` | زیر اشباع CPU، Writeهای کند شاید فشار مسیر خوانده شوند (**نامطمئن**) |
| 30 | `L24` در برابر مسیری که TCP را پس از چند KB می‌کشد | خنثی‌نشده | فقط هشدار یک‌باره (`:1440-1462`) | dial هر تیک؛ لینک‌ها کوتاه‌عمر؛ هیچ حلقه‌ای dial را کند نمی‌کند |

### ۷.۴ کدام خرابی را کدام حلقه می‌گیرد (لبهٔ مستقیم، پول چندلینکی)

| # | خرابی | می‌گیرد (اول ← بعد) | زمان | شکاف |
|---|---|---|---|---|
| F1 | سیاه‌چالهٔ لینک پرکار | `L19` ← `L17`، `L34` ← `L31` ← `L30` | ۸–۱۳ / ۱۲–۱۴ / ≈۲۰–۲۸ / ۲۴–۴۸s | بی‌شاهد stuck غیرفعال؛ کاربر تازه در ۶–۱۴s اول جذب می‌شود |
| F2 | سیاه‌چالهٔ لینک بیکار | `L17` ← `L31` | ≈۴–۱۴ / ≈۲۰–۲۸s | — |
| F3 | قطع یک‌طرفهٔ لبه→خروجی | `L31` و `L19`؛ خروجی `L30`/`L31` | ≈۲۰s | suspect نمی‌گیرد (NOP خروجی می‌رسد) |
| F4 | قطع یک‌طرفهٔ خروجی→لبه | `L17` و `L31` | ۱۲–۱۴ / ≈۲۰s | — |
| F5 | لینکی که پس از احراز هیچ قاب smuxی نمی‌گیرد | `L19` ← `L31` | ۸–۱۳ / ≈۲۰s | `L17` هرگز (`rxSeen=false`)؛ پس از infoDone قابل pick است |
| F6 | throttle DPI روی چند لینک | `L19` | ≈۸–۱۵s (سنجیده: ۹ از ۱۰ در ۱۰٫۶s) | اکثریت throttle ⇒ «مسیر کند»، هیچ تخلیه |
| F7 | ازدحام کل مسیر | `L20` (عمداً هیچ)، `L03` (شکست پروب) | فوری | طراحی |
| F8 | لینک پراتلاف پرکار | `L21` ← `L22` | ۴–۶ / ۹+s؛ سپس ۴۵/۹۰s | مسیر پراتلاف یا لینک با نرخ مسیر عمداً نگه داشته |
| F9 | **لینک پراتلاف کم‌حجم** (<96KiB/تیک) | **هیچ‌کس** مگر `ctrlWait ≥ 6s` | — | دروازهٔ `activeBytes` |
| F10 | **تأخیر ۲ تا ۵ ثانیه‌ای یک لینک** | **هیچ‌کس** | — | stuck ≥۶s لازم دارد؛ ۲–۶s «به هیچ سو شمرده نمی‌شوند» (`engine/stuck.go:39-43`)؛ pick و autopilot RTT نمی‌بینند |
| F11 | یک برنامه از خواندن بازمی‌ماند | عمداً هیچ‌کس؛ روی retiring `L10` پس از ۳۱۰s | — | — |
| F12 | ≥۴ برنامه روی یک لینک کاملاً از خواندن بازمی‌مانند | `L27` | ≈۴–۱۲s | — |
| F13 | **خوانندگان کند ولی زنده که سطل را خالی نگه می‌دارند** | **هیچ‌کس** (فقط دو لاگ ۱۰ دقیقه‌ای) | — | `L27` (Writeها کامل می‌شوند)، `L19` (معاف به‌خاطر wedged)، `L17`، `L30` (سطل ≤۰)، `L34` همه کورند؛ کل لینک به سرعت آن خواننده محدود و «سبک» دیده می‌شود |
| F14 | فشار حافظهٔ TCP هسته | `L28` ← `L27` | فوری؛ رله‌ها ≈۶–۸s | اگر علتش F13 باشد، کسی آزادش نمی‌کند |
| F15 | wedge سمت خروجی (پنل کند) | `L27` خروجی اگر کاملاً گیر؛ وگرنه `L19` لبه کل لینک را تخلیه می‌کند | ≈۴–۸ / ≈۸–۱۵s | هدف‌گیری دقیق ممکن نیست (`engine/stuck.go:55-58`) |
| F16 | قطعی کامل مسیر | `L17` ← `L31` ← `L24` ← `L15` | ۱۲–۱۴ / ≈۲۰–۲۸s؛ بازگشت ۱۶–۲۰s پس از قطعی ۴۰s (سنجیده) | stuck غیرفعال؛ کاربر تازه روی suspect؛ dial بی‌backoff |
| F17 | ری‌استارت پردازهٔ خروجی | `L31` (EOF) ← `L23` | ≤۲s | — |
| F18 | ری‌استارت لبه | `L11` و `L15` | ۱۶٫۳s (سنجیده) | — |
| F19 | مسیری که TCP را پس از ~۱۰KB می‌کشد | فقط هشدار `L23` | — | dial کند نمی‌شود؛ تغییر خودکار حامل نیست |
| F20 | همتا پایین است | مستقیم `L24` (epoch، لاگ ۳۰s)؛ معکوس `L25` | هر تیک / ۰٫۵–۸s | مستقیم backoff نمایی ندارد |
| F21 | کاربر تازه روی لینک گیرِ بی‌علامت | فقط `L31` یا `L33` | تا ۳۰s برای هر تلاش | `openStream` مهلت ندارد |
| F22 | جریان TUN روی لینک degraded/retiring/suspect | `L34` فقط با مرگ L3 | ۱۲–۱۴ / ۵ / ۳۰s | `l3Set.pick` کور |
| F23 | بار بیش از حد TUN | `L34` (۶۰ms، ۲۵۶) | فوری | طراحی: hs0 برای ترافیک سبک |
| F24 | **اشباع CPU** | **هیچ‌کس** در l3mtcp | — | `hostSaturated` فقط به udpcarrier می‌رسد |
| F25 | رشد حافظهٔ پردازه | فقط `L37` (GC) | — | سقف اتصال کاربر نیست (۱۰٬۰۰۰ اتصال ≈۰٫۹GB RSS، `CHANGELOG.md:1091-1093`) |
| F26 | هجوم به پولی که از قبل بالاست | فقط کلاهک `L12` | — | ≈۲۳۵ اتصال روی ۸ لینک اول (`CHANGELOG.md:1086-1090`) |
| F27 | پروب بالاتر از `max_links` خروجی (معکوس) | `L07` | ۱۵s+ تا ۸m | autopilot سقف همتا را نمی‌داند |
| F28 | retiring با اتصال‌های قطره‌ای | `L10` (۳۱۰s، ۲۰m) | — | اتصال flowing هرگز بسته نمی‌شود (طراحی) |
| F29 | گم شدن پیام pool-control | `L26` | ≤۳٫۶s | — |
| F30 | خروجی قدیمی بی pool-control یا kindStats | `L26` (`growable=false`)، `L14` (unsupported) | — | پول با تعداد ثابت |
| F31 | توقف VM یا پردازه | احتمالاً `L20` («مسیر کند») | — | مسیر stream `stallGate` ندارد؛ **نامطمئن** |

### ۷.۵ فهرست جمع‌بندی شکاف‌ها (فقط مشاهده)

1. **خوانندهٔ کند ولی زنده** (F13): بی‌نگهبان؛ کاربر تازه جذب می‌کند؛ ممکن است فشار حافظهٔ دائمی بسازد و همهٔ حکم‌ها را خاموش نگه دارد (ردیف‌های ۵، ۶، ۷، ۱۰).
2. **پراتلاف کم‌حجم** (F9) و **تأخیر ۲–۵ ثانیه** (F10): هیچ قاعده‌ای؛ `pickKey` و autopilot ورودی RTT یا loss ندارند (`engine/linkmanager.go:1470-1494`؛ `engine/autopilot.go:195-216`).
3. **جذب کاربر تازه به لینک خراب** پیش از علامت خوردن (ردیف ۴) و در قطعی کامل (ردیف ۳).
4. **TUN کور به سلامت لینک** (F22) و **حذف دائمی کانال L3** روی لینک زنده (ردیف ۸).
5. **`openStream` بی‌مهلت** (F21) و دروازهٔ info تا ~۳۵s.
6. **CPU** (F24) و **حافظهٔ پردازه** (F25) حلقهٔ واکنشی ندارند.
7. **dial مستقیم بی‌backoff** (F19، F20).
8. **سقف تخلیهٔ دوگانه** loss و stuck (ردیف ۱۱).
9. **suspect در سقف max** جایگزین نمی‌گیرد (ردیف ۲)؛ `heal` retiring را بی‌سنجش suspect برمی‌گرداند (ردیف ۱۴).
10. **کانال کنترل** پس از خطای غیر timeout هرگز باز نمی‌شود (`engine/control.go:142-143, 156-157`)؛ احتمال وقوع روی لینک زنده **نامطمئن**.
11. مسیر stream برخلاف dgtun `stallGate` ندارد (F31).

### ۷.۶ آزمون‌های حلقه‌ها: چه تضمین شده و چه نه

- **تضمین‌شده:** wedge (آزادسازی ۵ رلهٔ گیر، رلهٔ تنها آزاد نمی‌شود، خوانندهٔ قطره‌ای توقف را پنهان نمی‌کند ≤۱۲s، پایان رله با مرگ جریان؛ `engine/wedge_test.go:155-277`)؛ فشار حافظه (محلی و طرف مقابل، نبود حکم تا `stuckRecover`؛ `engine/mempressure_test.go:24-101`)؛ stuck (۲۰ آزمون از جمله `wedged by its own users`، `suspect: nothing received`، `every link waits`، کف throttle شبانه، مسیر شلوغ؛ `engine/stuck_test.go:216-1296`)؛ L3 (`engine/l3_link_test.go:43-224`)؛ آهنگ کنترل (`engine/control_cadence_test.go:71, 92`).
- **بدون آزمون مستقیم:** خوانندگان کند هم‌زمان؛ اثر suspect روی `dialRoom`/GROW/abort؛ لایهٔ ۲ pick در قطعی کامل؛ تعامل TUN با degraded/retiring؛ نبود بازگشایی L3/کنترل؛ مجموع تخلیهٔ loss+stuck؛ `openStream` روی نویسندهٔ گیر؛ توقف VM؛ تعارض stage و autopilot در dgtun.

---

## ۸. dgtun، حامل UDP، FEC و encap: خلاصه و مقایسه با l3mtcp

> **هیچ‌کدام از این‌ها روی مسیر دادهٔ l3mtcp نیستند.** `udpcarrier`، `fec`، `mmsg` و `encap` فقط از `engine/carrier_udp.go` (حامل‌های `udp`/`auto`)، `engine/dgcarrier.go` و `engine/dgpool.go` (dgtun) وارد می‌شوند؛ `engine` اصلاً `encap` را وارد نمی‌کند. تنها تماس l3mtcp: `defer encap.ReleaseAllEchoGuards()` در سه مسیر خروج (`cmd/hs2/main.go:325, 383, 1008`) که بی‌شنوندهٔ ICMP کاری نمی‌کند، و ساختار گزارشی مشترک `PoolStats`.

### ۸.۱ dgtun: نقش و چهار شکل اجرا

- `carrier: "dgtun"` ⇒ `runDgTun` (`cmd/hs2/main.go:420-421, 829-903`): «یک TUN روی **پول** حامل‌های datagram (udpcarrier روی encap انتخاب‌شده)، با همان autopilot پول جریانی، به‌علاوهٔ forwarder پورت در فضای کاربر». هر بستهٔ IP به‌صورت **یک datagram مهروموم‌شده** عبور می‌کند؛ پس **TCP-in-TCP نیست** (`engine/dgpool.go:18-44`). چند حامل چون throttle هر ۵-تایی هر حامل را جدا محدود می‌کند.
- TUN یک بار و **با offload** باز می‌شود و با مرگ حامل بسته نمی‌شود؛ MTU پیش‌فرض **۱۲۸۰** (`cmd/hs2/main.go:831-851`). سقف پیش‌فرض پول روی icmp **۸** (`icmpMaxLinks`، `:671-678`).

| نقش | dial/accept | autopilot | تابع | Phase |
|---|---|---|---|---|
| لبهٔ مستقیم | dial | بله | `RunDgEdge` → `runLoop(direct)` (`engine/dgpool.go:2187-2232, 2285-2307`) | فاز autopilot |
| خروجی مستقیم | accept | خیر | `RunDgExit` → `directExitTick` (`:2254-2274`) | `listening` |
| لبهٔ معکوس | accept | بله + `publishTarget` + `reconcileReverseEdge` | `:2217-2219, 2296-2302` | فاز autopilot |
| خروجی معکوس | dial | پیرو هدف لبه | `runReverseExit` (`:2276-2281, 2310-2337`) | `following` |

- **مسیر اتصال کاربر TCP:** کاربر → forwarder لبه روی `<user_listen_ip>:P` → **TCP تازه** به `<peer_tun_ip>:28443` (بی‌برچسب) یا `:28444` (برچسب پورت) → هستهٔ لبه به TUN → `pumpTun` → حامل → صف عادلانه → `writeLoop` → `udpcarrier.Conn` (Noise + FEC + pacer + encap) → سیم → `readLoop` → `reorderer` → `tunBatch` → TUN خارج → listener روی `<local_tun_ip>:28443/28444` → dial پنل. **سه پای TCP**؛ پای میانی بافر دریافت ثابت ۴MB دارد (`engine/dgforward.go:14-83`؛ `engine/dgports.go:16-49`).

### ۸.۲ dgtun: مسیر داده و سازوکارها

| سازوکار | عدد / رفتار | path:line |
|---|---|---|
| انتخاب حامل | flowlet چسبنده: مکث <`flowletGap`=300ms ⇒ همان حامل؛ وگرنه rendezvous `mix32(flow ^ id)` با ۴ طبقه (serving / retiring یا peerRetiring / serving+avoid / retiring+avoid) | `engine/dgpool.go:978-1043` |
| صف هر حامل | DRR (`fq`) ۲۵۶ بسته؛ quantum ۱۵۰۰؛ flow تُنُک اول (≤۳۲KB/s، burst ۸KB)؛ صف پر ⇒ دورریز سر **چاق‌ترین** flow | `engine/dgfq.go:49-63, 76-176`؛ `engine/dgpool.go:366-377` |
| sojourn | `dgSojourn`=50ms ⇒ دورریز + **ثبت فشار** + `NoteQueueDrop` | `engine/dgpool.go:50, 418-467` |
| خط سریع | `SendUrgent` با حفظ ترتیب از راه `LaneMark/LaneDrained` | همان |
| دریافت | دسته تا ۶۴ قاب؛ `reorderer` (فقط TCP دارای داده؛ نگه‌داشت ۱۵ms؛ سرریز ۲۵۶/flow یا ۴۰۹۶/حامل) → `tunBatch` با GRO | `engine/dgpool.go:725-810`؛ `engine/reorder.go:197-279`؛ `engine/tunbatch.go:5-17` |
| mute | `muteLoop` هر 250ms؛ حامل ساکت ≥1s وقتی دیگری می‌شنود ⇒ mute و دو `closeMute`؛ ≥3s ⇒ `closeLink`؛ همتا `avoid` تا 4s یا `closeHear`؛ `stallGate`: تیک دیرتر از ۲ دوره و ۵۰۰ms بعدش قضاوت نمی‌شود | `engine/dgpool.go:1047-1142, 303-309` |
| scout / zombie | همه ≥3s ساکت ⇒ یک dial پیشاهنگ هر ≥5s؛ حامل تازه ساکت‌های ≥3s را می‌کشد | `:1342-1386, 674-715` |
| retire آینه‌ای | `closeRetire`/`closeServe`؛ یادآوری ≥3s؛ انقضای ۱۲s؛ `dgRetireForce`=30s؛ بستن retiring بی‌flow در ۳۰۰ms اخیر؛ spare ۳۰s؛ لبهٔ معکوس ۶s | `:274-293, 337-347, 1551-1604` |
| فشار | `pressed = !gov.Capped && !retiring && warm && droppedAt در همین تیک` (warm = `Warm()` یا ۴s) | `:1191-1198` |
| فشار دانلود | `TypeLinkStats`: `[pressed][serving][ceiling]`؛ لبه همان تعداد پرکارترین حامل‌ها را pressed می‌زند؛ کهنگی ۶s | `:1224-1263, 2029-2052` |
| dial | بودجه `min(max(4, ceil(T/16)), 20)` و ≤ `max − count − dialing`؛ همان `linkGate` سراسری | `:1418-1534` |
| حامل مرده | `deadAfter`=15s بی‌دریافت | `udpcarrier/carrier.go:54, 318-323` |
| مسیریابی هر پورت | probe روی ۲۸۴۴۴ (۳s؛ هر ۱۵s، در unknown هر ۲s)؛ سرآیند `[1][port u16]` | `engine/dgports.go:162-579` |
| echo 1:1 روی icmp (B5) | پرکنندهٔ Ping/Pong ۰..۹۵ بایت در جهت سبک‌تر | `engine/dgpool.go:887-925` |

### ۸.۳ حامل UDP (`udpcarrier`) و FEC

- **چرا:** مسیر هدف ۲۶٪+ اتلاف انفجاری دارد؛ TCP آن را ازدحام می‌خواند. این حامل بازارسال ندارد، با parity بازسازی می‌کند و نرخ را از مدل پهنای باند/تأخیر تنظیم می‌کند؛ هدف «تأخیر و jitter صاف» است (`udpcarrier/doc.go:7-16`؛ `udpcarrier/REPORT.md:5-9`). لایه‌ها: `udpcarrier.Conn` → `fec` (Reed-Solomon درهم‌چیده) → `encap` → `mmsg` (sendmmsg/recvmmsg).
- **کنترل نرخ تأخیرمحور** (`udpcarrier/rate.go`): نرخ اولیه ۱۲۵۰۰۰B/s، کف ۳۲۰۰۰B/s؛ startup با ضریب ۲٫۸۸۵ (BBR)؛ `targetQueue`=10ms، `lowQueue`=5ms؛ `rate = capEst × clamp(1 + (0.010 − q)/τ, 0.5, 1.1)` با `τ = max(250ms, 2.5·srtt)`؛ رشد ظرفیت ۴٪ در هر گزارش بی‌صف؛ base probe هر ۴s روی ساعت مشترک پول (`0.75×` نرخ، ≈۲٪ هزینه)؛ جبران اتلاف تصادفی `1/(1−min(loss,0.5))`؛ کشف policer تک‌حاملی (`fullLoss > probeLoss + 0.05` و ۲s بی‌صف ⇒ سقف `1.1×mean(delivery)`)؛ قاعدهٔ stage (CPU/سوکت عقب) ⇒ `capEst ≤ 2×` آنچه بیرون رفته (`:225-300, 430-879`).
- **pacer** (`udpcarrier/pacer.go`): بودجهٔ صف `max(rate×20ms, 4500B)`؛ سطل توکن `rate×2ms` (حداقل دو datagram)، `10ms` برای حامل stage-limited، `50ms` وقتی میزبان اشباع است (`hostSaturated`)؛ سه صف (اولویت parity، فوری، داده) (`:82-120, 138-148, 299-337`).
- **Governor** (پولی، هر 500ms؛ `udpcarrier/governor.go:108-133`): episode = ≥۲ حامل pushing، اتلاف ≥۵٪ و ≥۳×میانه+۳٪، صف <۵ms، ≥۶۰٪ حامل‌ها lossy ⇒ سقف `0.9 × passedRate`؛ بالا بردن ۱۵٪ هر ۴s؛ استراحت ۵m تا ۱h. زیر سقف Governor هیچ حاملی pressed نیست ⇒ رشد autopilot عمداً خنثی (`engine/dgpool.go:1159, 1198`).
- **FEC** (`fec/*`؛ حامل `K=8`، `Window=60ms`، `MaxDepth=64`، TTL decoder ۲۲۰ms، `udpcarrier/carrier.go:162-187`): parity کوچک‌ترین r با باقیماندهٔ ≤۱٪ و سقف `ceil(1.5k)`=۱۲؛ Adapter نامتقارن (بالا ۰٫۵، پایین ۰٫۰۸، حفظ اوج ۲s، `+0.02`، کف ۰٫۰۳، سقف ۰٫۵). جدول r برای K=8: برآورد ۰ ⇒ r=1 (۱۲٫۵٪)، ۰٫۰۷ ⇒ ۳، ۰٫۱۵ ⇒ ۵، ۰٫۲۵۵ ⇒ ۸ (۱۰۰٪)، ۰٫۳۶۵ ⇒ ۱۲ (۱۵۰٪؛ محاسبه‌شده). گروه کوچکی که با پنجره بسته شود دست‌کم یک parity دارد (k=1 ⇒ ۱۰۰٪ سربار).
- **MTU:** `CarrierOverhead`=56؛ `InnerMTUFor`: udp/gre ۱۴۱۶، icmp ۱۴۰۸، ipip/ipx ۱۴۲۰ (`udpcarrier/mtu.go:11-32`).
- **`auto`:** کاوش احرازشدهٔ UDP (n=16، 8ms، 300ms) و اگر `Reachable && Loss ≤ 0.45` ⇒ UDP؛ وگرنه **`noise` روی TCP** (نه TLS) (`engine/carrier_udp.go:24, 57-90`). تصمیم فقط هنگام dial. **مشاهده:** آستانه روی اتلاف رفت‌وبرگشت اعمال می‌شود؛ ۲۶٪ یک‌طرفه ≈ ۴۵٪ رفت‌وبرگشت، یعنی درست روی مرز (محاسبه‌شده).
- **موتور تک‌حاملی** (`engine/engine.go`) برای `reality`، `noise`، `udp`، `auto`: یک حامل در هر لحظه؛ TUN یک بار باز؛ dial با backoff ۵۰۰ms دوبرابرشونده تا ≈۸s؛ keepalive ۵s، مرگ ۱۵s؛ حامل تازه جای قبلی را می‌گیرد؛ بی‌حامل ⇒ دورریز بی‌شمارش (`:48-198`). از آن فقط `acceptBackoff` و `sleepCtx` در l3mtcp به کار می‌رود.

### ۸.۴ encap

- پنج کپسول (`encap/encap.go:43-49`): `udp` (سوکت عادی، بافر ۴MB)، `icmp` (Echo؛ پروتکل ۱)، `gre` (۴۷، RFC 2890 با key)، `ipip` (۴)، `ipx` (پروتکل خام انتخابی، پیش‌فرض ۲۵۳). خام‌ها `CAP_NET_RAW`، فقط لینوکس و **فقط IPv4**. سربار: udp ۸، icmp ۱۶ (۸ سرآیند + ۸ nonce)، gre ۸، ipip ۴، ipx ۴ (`encap/raw.go:25-30`).
- **ICMP همیشه مبهم است:** magic ثابت نیست؛ پیشوند کلیددار جهت در بایت بالای seq و شمارندهٔ ۸بیتی؛ nonce هشت‌بایتی هر بسته؛ ماسک ChaCha20 روی ۶۴ بایت اول payload؛ نوع/checksum/DF مثل ping؛ شمارندهٔ پاسخ مستقل با بذر تصادفی (`encap/obfs.go:74-181`؛ `encap/rawframe.go:78-212`). gre/ipip/ipx: magic کلیددار HMAC برای هر نوع/جهت/پروتکل.
- **سوکت:** یک سوکت دریافت مشترک برای هر همتا (`rawMux`) با demux شناسهٔ لینک ۱۶بیتی؛ سوکت شماره‌گیر **نامتصل** + فیلتر BPF کلیددار؛ `recvmmsg/sendmmsg` دسته‌های ۳۲؛ سوکت فقط‌ارسال `IPPROTO_RAW` فقط برای icmp (X5/X7) (`encap/raw_linux.go:165-336`؛ `encap/rawtx_linux.go:53-277`).
- **echoguard:** قاعده‌ای که فقط پاسخ‌های هستهٔ همان درخواست‌های تونل (پیشوند c2s) را دور می‌ریزد تا پینگ عادی کار کند؛ ترتیب `nft` ← `iptables u32` ← `global` (`icmp_echo_ignore_all`)؛ نام‌گذاری با PID، sweep در شروع، `ReleaseAllEchoGuards` در خروج (`encap/echoguard_linux.go:41-379`). **این قاعده را باینری می‌گذارد**؛ نصب‌کننده فقط بسته‌های `iptables`/`nftables` را نصب می‌کند (اصلاح ۹۱ ردیف ۷۴).
- **استتار اختیاری (`HS2_ICMP_CAMO=1`، فاز CA):** CA1 بازخورد با لرزش ۶۰–۱۴۰ms؛ CA1b ضربان بیکاری ≈۰٫۷s (۵۶۰–۸۴۰ms) به‌جای سکوت کامل (سکوت کامل با آشکارساز mute ۱s تداخل داشت: ≈۳۹ mute در ۲٫۵ دقیقه)؛ لرزش قطعی ±۱٫۵s کاوش پایه؛ CA2 شناسهٔ لینک شبه‌PID خوشه‌ای (`udpcarrier/carrier.go:35-47, 537-583`؛ `udpcarrier/rate.go:345-382`؛ `encap/raw_linux.go:42-49`).
- **محدودیت‌های اعلام‌شده:** حجم پنهان نمی‌شود (یک جفت IP با هزاران echo خودش نشانه است)؛ قالب icmp با نسخه‌های قدیمی سازگار نیست؛ هسته پیش از دورریز پاسخ را می‌سازد (`README.md:606-646`). جدول همتاهای شنوندهٔ خام سقف ۱۶۳۸۴ دارد و اگر جاروب ۱۰ ثانیه‌ای هم کم نکند کل جدول خالی می‌شود (`encap/raw_linux.go:65-70, 1017-1021`).

### ۸.۵ مقایسهٔ dgtun و l3mtcp

| جنبه | dgtun | l3mtcp |
|---|---|---|
| حمل TCP کاربر | پای میانی = TCP مستقل هر کاربر داخل datagramها | جریان smux روی لینک TLS/TCP مشترک (پنجرهٔ smux، HOL بین کاربران یک لینک) |
| TUN | مسیر اصلی؛ offload؛ MTU ۱۲۸۰ | کانال جانبی؛ بی‌offload؛ MTU ۱۳۸۰ |
| واحد جای‌گذاری | بسته/flow پنج‌تایی: rendezvous + sticky | اتصال کاربر با `Pick` **آگاه از بار**؛ hs0: rendezvous **ساده، بی‌sticky** |
| آگاهی از فشار در جای‌گذاری | ندارد | دارد (`pickKey.pressed`) |
| صف ارسال | DRR + خط سریع + دورریز چاق‌ترین؛ ۲۵۶؛ ۵۰ms | جریان‌ها: پس‌فشار smux؛ hs0: FIFO ۲۵۶، دورریز تازه‌وارد، ۶۰ms، دسته ۱۶KiB |
| مرتب‌سازی | reorderer ۱۵ms | ندارد (TCP مرتب؛ hs0 بی‌بازچین) |
| نوشتن TUN | `tunBatch` + GRO | `dev.Write` تک‌به‌تک |
| اتلاف | FEC تطبیقی + Governor | بازارسال TCP هسته + degrade (>۱۲٪) و جایگزینی |
| جایگزینی لینک پراتلاف/کند | ندارد | دارد (`heal`، make-before-break) |
| زنده‌بودن | بازخورد ۱۰۰ms ⇒ mute ۱s، silent ۳s، deadAfter ۱۵s، scout، zombie | keepalive smux، suspect ۱۲s، L3: ۵s/۱۲s/۳۰s، wedge/stuck |
| اطلاع قطع یک‌طرفه به همتا | `closeMute/closeHear` | سازوکار صریحی دیده نشد (**نامطمئن**) |
| سیگنال فشار | دورریز صف/کهنه در همین تیک (تک‌رخدادی) | نویسندهٔ مسدود ≥۵۰٪، ≥16KiB، rwnd<۵۰٪، ۲ از ۳ |
| فشار دانلود | `TypeLinkStats` تجمیعی (فقط تعداد) | `kindStats` رکورد هر لینک |
| retire | flowlet ۳۰۰ms، اجبار ۳۰s، بی‌بستن اتصال کاربر | تا پایان اتصال‌ها؛ `drainIdle` ۳۱۰s؛ `retireForce` ۲۰m |
| churn guard معکوس | فقط `dgRetireHold` و `bornSpareGrace` | `churnTrips/churnWindow/churnHold` (`engine/linkmanager.go:95-104`) |
| refill hold | ندارد | دارد |
| `growable` | همیشه `true` | در معکوس بی pool-control ⇒ `false` |
| وضعیت | `Pressed/Saturated/CapMbit/NextProbeS/HeldBy` پر نمی‌شوند؛ شمارندهٔ دورریز به تفکیک علت و `tun_*` دارد | این فیلدها پر می‌شوند؛ دورریز hs0 فقط یک خط لاگ ۳۰ ثانیه‌ای |
| `stallGate` (توقف VM) | دارد | ندارد |

- **در dgtun هست و به l3mtcp سیم‌کشی نشده:** offload TUN، `tunBatch`، `reorderer`، جدول sticky با flowlet ۳۰۰ms، صف عادلانه (`dgfq.go`)، اعلام صریح mute/hear، zombie-on-fresh-link، `stallGate`، شمارنده‌های دورریز به تفکیک علت و فیلدهای `tun_*` (فقط در `engine/dgpool.go:670-671` ساخته می‌شوند).
- **در l3mtcp هست و dgtun ندارد:** جای‌گذاری آگاه از فشار/بار، degrade/heal، فشار با حداقل بایت و چسبندگی ۲ از ۳، churn guard معکوس، `growable=false`، refill hold، لاگ دوره‌ای دورریز، فیلدهای `Pressed/CapMbit/...` در وضعیت.
- **مشاهده‌های مهم dgtun** (`08` §۱۴): جای‌گذاری از فشار بی‌خبر؛ فشار تک‌رخدادی؛ نبود جایگزینی حامل پراتلاف؛ احتمال چرخهٔ churn ≈۳۰ ثانیه‌ای در معکوس وقتی `min_links` خروجی از هدف لبه بیشتر است؛ جابه‌جایی flow بین حامل‌ها بازچین ندارد (README می‌گوید «a pool resize never moves a live flow»، `README.md:471`، ولی کد پس از ۳۰s retire و هنگام mute جابه‌جا می‌کند)؛ یک دورریز صف را autopilot «حامل بیشتر» و کنترل نرخ «سقف محلی» می‌خوانند (اثر **نامطمئن**).

---

## ۹. پیکربندی، متغیرهای محیطی، نصب و ابزارهای عملیاتی

### ۹.۱ کلیدهای فایل پیکربندی (`fileConfig`، `cmd/hs2/main.go:36-105`)

| کلید | پیش‌فرض / رفتار خالی | نقش | در l3mtcp |
|---|---|---|---|
| `mode` | — (`check` فقط `dial`/`listen`) | `dial` = ایران/لبه، `listen` = خارج/خروجی. **`run` آن را اعتبارسنجی نمی‌کند**: مقدار غلط بی‌صدا «خروجی» تعبیر می‌شود (`main.go:468`) | بله |
| `carrier` | خالی = `noise` | `l3mtcp` یا `l3` ⇒ `runStream(true,0)` | بله |
| `addr` | — | مقصد dial یا نشانی گوش‌دادن | بله |
| `reverse` | false | فقط جهت TLS را عوض می‌کند: `dialing = (mode=="dial") != reverse` (`main.go:430, 907`) | بله |
| `iface` / `local_cidr` / `peer_ip` | خالی | TUN؛ **تهی بودنشان را `check` نمی‌سنجد** (`check.go:333-343`) | بله |
| `mtu` | 0 ⇒ ۱۳۸۰ (stream)، ۱۲۸۰ (udp/auto/dgtun) | MTU hs0 | بله |
| `min_links` / `max_links` / `per_link` | ۲ / غایب=۳۲، `0`=خودکار، عدد=ثابت / ۸ | پاکت پول (۹.۲) | بله (`per_link` فقط لبه) |
| `drain_idle_sec` | غایب=۳۱۰، `0`=هرگز | بستن اتصال بیکار روی retiring | لبه |
| `forward_ports` | خالی ⇒ هشدار «تونل مسیریابی خالص» | پورت‌های کاربر روی لبه | بله |
| `udp` | false | پذیرش UDP روی پورت‌های کاربر | بله |
| `user_listen_ip` / `bind_local_ip` | خالی | IP شنوندهٔ کاربر / IP مبدأ dial (نامعتبر ⇒ خروج) | بله |
| `expose` / `port_map` | خالی | پنل پیش‌فرض / مقصد اختصاصی هر پورت (روی خروجی) | خروجی |
| `sni` | خالی (هشدار DPI در `check`) | SNI دست‌دهی | بله |
| `backend_addr` / `cover_seed` | `builtin` / خالی ⇒ صفحهٔ قدیمی ثابت | سایت پوششی سمت سرور TLS | بله |
| `shared_key` | — (۳۲ بایت هگز) | کلید مشترک؛ **`unhex` خطای هگز را نادیده می‌گیرد** (`main.go:1005`) | بله |
| `cert_file` / `key_file` | — | فقط سمت سرور TLS | بله |
| `tuning` | غایب = auto | `mode` (auto/manual/off)، `congestion`، `qdisc`، `rmem_max`، `wmem_max`، `netdev_backlog`، `somaxconn` (`tune/tune.go:41-52`) | بله |
| `encap`، `proto` | — | فقط dgtun | خیر |
| `local_priv`/`local_pub`/`remote_static`/`psk`، `cover_addr` | — | فقط noise / reality | خیر |
| `peer_panel` | — | فقط اطلاعاتی؛ در کد استفاده نمی‌شود | — |

`hs2 config -c cfg get|set|unset` فقط فهرست سفید را ویرایش می‌کند: `min_links`، `max_links` (`auto` ⇒ ۰)، `per_link`، `drain_idle_sec`، `forward_ports`، `port_map`، `expose`، `udp`، `tuning.*` (`cmd/hs2/config.go:39-59`)؛ کل فایل با `checkConfig` سنجیده و در صورت **خطا** ذخیره نمی‌شود؛ نوشتن اتمی با `0600` و بی fsync؛ ترتیب کلیدها الفبایی می‌شود. کلیدهای TUN، `carrier`، `addr`، `sni` و `reverse` با این دستور ویرایش‌پذیر نیستند. **هیچ بارگذاری زنده‌ای جز گواهی (SIGHUP) نیست**؛ هر تغییری پس از ری‌استارت اثر دارد.

### ۹.۲ پاکت پول و سقف لینک

```
linkCeiling(fc)                         cmd/hs2/main.go:711-724
  max_links > 0          → max_links      "fixed"
  dgtun روی icmp         → 8              "icmp"
  max_links == 0 (صریح)  → RecommendedMaxLinks(ram, cpus)   "auto"
  غایب / null            → 32             "default"
linkEnvelope(fc)                        cmd/hs2/main.go:644-657
  min = min_links || 2 ; per = per_link || 8 ; max = max(linkCeiling, min)
maxLinksRule(ram, cpus)                 tune/tune.go:235-251
  base = 32 | 48 | 64 (پروفایل) ; cpus<2 → base ; byRAM = ram/48
  byRAM>300 → 300 ; cpus<4 && byRAM>128 → 128 ; byRAM ≤ base → base ; else byRAM
```

- `LinkWorstCaseMiB=12` (۸MiB سطل smux × ۱٫۵)، `LinkRAMPerLinkMB=48` (تا بدترین حالت ≤۲۵٪ RAM)، `MaxLinksCap=300`، `maxLinksFewCores=128` (`tune/tune.go:198-213`). RAM از `MemTotal` و محدودیت cgroup (v1/v2)؛ هسته = `min(NumCPU, GOMAXPROCS)`.
- نمونه‌ها (اجراشده با کد): ۱۰۲۴MB/۱ هسته ⇒ ۳۲؛ ۲۰۴۸/۲ ⇒ ۴۸؛ ۳۸۰۰/۲ ⇒ ۷۹ (پروفایل medium)؛ ۸۱۹۲/۲ ⇒ ۱۲۸ (few cores)؛ ۸۱۹۲/۴ ⇒ ۱۷۰؛ ۱۴۴۰۰/۴ ⇒ ۳۰۰؛ سرورهای تولید ۱۷۴۰۸/۲۰ و ۲۲۵۲۸/۱۲ ⇒ ۳۰۰ (cap).
- **سقف مؤثر** (`effectiveCeiling`، `cmd/hs2/status.go:584-606`، فقط نمایشی): مستقیم = سقف ایران؛ معکوس = کمینهٔ دو سرور. سقف فقط در شروع حل می‌شود.
- نصب‌کنندهٔ تازه `max_links: 0` (خودکار)، `min_links: 2`، `per_link: 8` می‌نویسد (`install.sh:1778-1790`).
- **فایل warm** (`<config>.warm` در `/run/hs2`): پس از ۱ دقیقه کارکرد نوشته می‌شود، کهنه‌تر از ۱۵ دقیقه نادیده گرفته می‌شود، و فقط اگر از `WarmSize(min,max)` بیشتر باشد اندازهٔ شروع را **بالا** می‌برد (اصلاح ۹۱ ردیف ۷۱؛ `cmd/hs2/status.go:192-246`؛ `cmd/hs2/main.go:280-293`). لاگ: `link pool: coming up at %d links, the size it had before this restart …`.

### ۹.۳ متغیرهای محیطی

| متغیر | پیش‌فرض | اثر | path:line |
|---|---|---|---|
| `HS2_TUNE_NOTSENT` | 32768 | `TCP_NOTSENT_LOWAT` سوکت لینک؛ **معنای فشار آپلود به آن وابسته است** | `cmd/hs2/main.go:267` |
| `HS2_TUNE_SMUX_FRAME` / `_STREAMBUF` / `_SESSBUF` | 16KiB / 2MiB / 8MiB | اندازه‌های smux (فقط محلی؛ سقف لینک را عوض نمی‌کند) | `cmd/hs2/main.go:268-270` |
| `HS2_TUNE_CC` | bbr (پروفایل) | کنترل ازدحام سوکت لینک | `cmd/hs2/main.go:271-274, 357` |
| `HS2_NO_TUNE` | — | sysctl اعمال نمی‌شود | `cmd/hs2/main.go:352` |
| `GOMEMLIMIT` | نصف RAM | حد نرم حافظهٔ Go | `cmd/hs2/main.go:305-320` |
| `HS2_PPROF` | — | pprof فقط روی loopback | `cmd/hs2/pprof.go:21` |
| فقط datagram: `HS2_TUN_OFFLOAD`، `HS2_DG_FQ`، `HS2_FAIR_SHARE`، `HS2_DG_PAD`، `HS2_ICMP_CAMO`، `HS2_RAW_BATCH`، `HS2_RAW_TX`، `HS2_ICMP_SUPPRESS`، `HS2_TUN_RCVBUF`، `HS2_TUN_REORDER_MS` | — | بی‌اثر روی l3mtcp | نقشه‌های ۰۸–۱۰ |
| نصب‌کننده: `HS2_REPO_RAW`، `HS2_YES`، `HS2_ALLOW_UNVERIFIED`، `HS2_VERIFY_SECS` (پیش‌فرض ۶۰) | — | منبع دانلود، اجرای بی‌ناظر، نصب بی‌sha256، مهلت `verify_tunnel` | `install.sh:24, 902` |

**هیچ‌کدام از ثابت‌های autopilot، loss، stuck، wedge یا dial gate از پیکربندی یا محیط تنظیم‌پذیر نیستند.**

### ۹.۴ تنظیم هسته (`tune`، فقط یک بار در شروع)

- در هر `hs2 run`، اگر root باشد و `HS2_NO_TUNE` خالی و `tuning.mode != off`، Plan ساخته و اعمال می‌شود (`cmd/hs2/main.go:351-362`؛ `tune/tune.go:294-448`). **اعمال دوره‌ای نیست**؛ تغییر بعدی sysctlها تا ری‌استارت برگردانده نمی‌شود (doctor فقط ناهمخوانی را نشان می‌دهد). sysctlها هنگام توقف/حذف برگردانده نمی‌شوند.
- پروفایل: high اگر `ram≥4096 && cpus≥4` یا `ram≥4096 || (ram≥2048 && cpus≥4)`؛ medium اگر `ram≥1536`؛ وگرنه low (`tune/tune.go:159-175`). بافر `rmem_max/wmem_max`: ۸/۱۶/۳۲MiB؛ backlog/somaxconn: ۲۰۴۸/۱۰۲۴، ۸۱۹۲/۴۰۹۶، ۱۶۳۸۴/۸۱۹۲.
- مقادیر ثابت: `tcp_congestion_control=bbr` (← cubic)، `default_qdisc=fq_codel` (← fq)، `tcp_rmem="4096 131072 max"`، `tcp_wmem="4096 65536 max"`، `tcp_notsent_lowat=131072` (سیستمی؛ سوکت لینک ۳۲KiB خودش را دارد)، `tcp_slow_start_after_idle=0`، `tcp_mtu_probing=1`، `tcp_fin_timeout=20`، `tcp_tw_reuse=1`، `rp_filter=2` (loose)؛ جمعاً ۱۶ sysctl در حالت auto (`tune/tune.go:338-370`).
- `ip_local_port_range` **عمداً دست نمی‌خورد** و فقط اگر دقیقاً مقدار قدیمی hs2 (`10240 65535`) باشد به پیش‌فرض برگردانده می‌شود (`tune/tune.go:545-561`).
- **مشاهده:** فقط `default_qdisc` نوشته می‌شود و هیچ `tc qdisc replace` نیست؛ پس qdisc رابط فیزیکی (eth0) همان زمان بوت می‌ماند، ولی hs0 (که پس از tune ساخته می‌شود) از آن پیروی می‌کند. qdisc واقعی hs0 **نامطمئن**.
- **سوکت لینک** (مستقل از sysctl، هر دو سمت): `TCP_NODELAY`، `NOTSENT_LOWAT=32KiB`، `USER_TIMEOUT=20s`، CC هم‌راستا با Plan؛ keepalive سه‌ثانیه‌ای فقط سمت dialer و فقط `TCP_KEEPIDLE`؛ MPTCP خاموش (`godebug multipathtcp=0` و `engine/listen.go:41`).

### ۹.۵ نویسندهٔ وضعیت و سیگنال‌های کنترلی آن

- هر ۲ ثانیه `/run/hs2/<config-path>.status.json` اتمی نوشته می‌شود (`cmd/hs2/status.go:29, 250-349`)؛ با خاموشی پاک می‌شود؛ فایل warm می‌ماند.
- **نویسندهٔ وضعیت فقط نمایش نیست؛ دو سیگنال مسیر داده را می‌سازد:** `engine.SetTCPMemPressure` (`mem ≥ tcp_mem[1]` روشن، `< 0.9·tcp_mem[1]` خاموش؛ `status.go:840-855`) و `udpcarrier.SetHostSaturated` (فقط datagram؛ `status.go:330`). **اگر `/run/hs2` ساخته نشود، هر دو بی‌صدا غیرفعال‌اند** (`status.go:252-254`).
- `cpuMeter` (CPU خود فرایند؛ ≥۹۰٪·cores سه نمونه ⇒ لاگ، <۷۰٪ خاموش) و `hostMeter` (busy ≥۹۰ یا PSI10 ≥۴۰ سه بار ⇒ اشباع؛ busy <۷۵ و PSI <۲۰ پنج بار ⇒ پایان) (`status.go:355-404`؛ `hostcpu.go:58-242`). در l3mtcp **فقط لاگ و نمایش**اند.
- حامل‌های تک‌نشستی (`udp`/`auto`/`noise`/`reality`) نویسندهٔ وضعیت ندارند.

### ۹.۶ دستورهای `hs2`

| دستور | کار | path |
|---|---|---|
| `hs2 run -c cfg` | دیمن (از unit) | `cmd/hs2/main.go:322-424` |
| `hs2 status [--watch]` | داشبورد از فایل وضعیت؛ «کهنه» اگر >۶s | `cmd/hs2/status.go:422-536` |
| `hs2 doctor -c` | ۱۵ بررسی فقط‌خواندنی: binary، config، running(+refill)، endpoint (TCP ۸s)، certificate، cert renewal، `tun <iface>`، kernel tuning، link pool ceiling، link pool (other server)/visibility، user ports، kernel TCP memory، conntrack (≥۷۰٪)، server cpu (`busy≥90 \|\| psi60≥40`)، tunnels together (>۴۰٪ RAM)، clock؛ خروج ۱ فقط با `fail` | `cmd/hs2/doctor.go:29-697`؛ `doctor_cert.go` |
| `hs2 check -c` | کلید ناشناختهٔ سطح بالا (نه زیر `tuning`)، `mode`، حامل، IPهای محلی، `shared_key`، SNI خالی، گواهی سمت سرور، `iface ≤ 15`، `mtu` خارج از ۵۷۶..۹۰۰۰ (هشدار)، مرزهای پول (منفی/>۶۵۵۳۵ خطا، `min>max` خطا، >۱۰۲۴ هشدار، `min_links>64` هشدار)، `drain_idle_sec` ۰..۳۰۰ هشدار؛ نام cc/qdisc بررسی نمی‌شود؛ خروج ۱ فقط برای ERROR | `cmd/hs2/check.go:31-477` |
| `hs2 config` | ۹.۱ | `cmd/hs2/config.go:16-242` |
| `hs2 ports` | جدول پورت‌های دو سمت و ویرایش همه-یا-هیچ | `cmd/hs2/ports.go` |
| `hs2 tune [--apply]` | نمایش Plan؛ **فقط‌خواندنی نیست** (`AvailableCC/Qdisc` ممکن است `modprobe` کنند) | `cmd/hs2/main.go:572-592`؛ `tune/tune.go:486-513` |
| `hs2 recommend-links [--why] [-c]` | سقف پیشنهادی | `cmd/hs2/main.go:604-630` |
| `hs2 cleanup` | پاک‌سازی قواعد echoguard یتیم | `cmd/hs2/cleanup.go:20-32` |
| `hs2 version` | `hs2 v3 (…) [build …]` | `cmd/hs2/main.go:127, 221-222` |

خاموشی: SIGINT/SIGTERM ⇒ لغو ctx ⇒ اگر تا ۳s خارج نشد `os.Exit(0)` (همیشه کد ۰)؛ systemd پس از ۸s KILL. `must(err)` ⇒ آزادسازی echoguard و `log.Fatal` ⇒ systemd پس از ۳s ری‌استارت. خطای bind پورت کاربر کل دیمن را می‌کشد (`engine/stream_iran.go:141-144`).

### ۹.۷ نصب‌کننده (`install.sh`، ۴۲۵۴ خط؛ نسخهٔ `hs2-src/install/install.sh` بایت‌به‌بایت یکسان)

- **نقش‌ها:** «یک باینری مشترک (`/usr/local/bin/hs2`)، چند سرویس» (`hs2` و `hs2-<name>`، پیکربندی در `/etc/hs2/`). جادوگر خارج/ایران؛ شنونده لینک `hs2://` می‌سازد، شماره‌گیر آن را می‌چسباند (`install.sh:1689-1692`).
- **تقسیم کار** (مهم برای پرهیز از دوباره‌کاری): نصب‌کننده فقط بسته‌ها (`iproute2 iptables curl ca-certificates`، `nftables iputils-ping`)، `modprobe tun`/`tcp_bbr` و پایدارسازی `tcp_bbr`، و حذف فایل قدیمی `99-hs2.conf` را انجام می‌دهد. **همهٔ sysctlها، رابط TUN (نشانی، MTU، `txqueuelen 2000`، route `/32`)، قاعدهٔ echoguard، سقف خودکار، بارگذاری گواهی و اعتبارسنجی** کار باینری است. **فایروال، NAT، `ip_forward`، MASQUERADE را هیچ‌کدام تنظیم نمی‌کنند** (`install.sh:278-307`؛ اصلاح ۹۱ ردیف ۷۳–۷۴).
- **دو راه ساخت l3mtcp** (اصلاح ۹۱ ردیف ۷۶):
  - **راه الف** «tun → 6) tcp → 1) mtcp + tun» (رسمی؛ متن منو: «The tun here is a side channel for ping and light traffic, not for bulk», `install.sh:1440-1443`): `ask_tun_mtu` با پیش‌فرض **۱۳۲۰** و بازهٔ ۶۸..۶۵۵۳۵ (`install.sh:1660-1672, 1901, 2276`)؛ نام رابط پرسیده می‌شود؛ پورت کاربر اختیاری («pure routed tun»).
  - **راه ب** «tcp → TLS mode 2) l3mtcp»: **ثابت ۱۳۸۰** (`install.sh:1875, 1980, 2103, 2239`)؛ رابط خودکار؛ پورت کاربر اجباری.
  - در معکوس، سمت شماره‌گیر MTU را از لینک می‌گیرد (`install.sh:2002, 2132`). بی `mtu` در پیکربندی ⇒ پیش‌فرض باینری ۱۳۸۰.
- **قالب پیکربندی l3mtcp مستقیم:** خارج `"mode": "listen", "carrier": "l3mtcp", "addr", "iface", "local_cidr": "<base+2>/30", "peer_ip": "<base+1>", "mtu", "backend_addr": "builtin", "cover_seed", "shared_key", "cert_file", "key_file", "expose"[, "port_map"]` (بی min/max/per)؛ ایران `"mode": "dial", "udp", "addr", "sni", "iface", "local_cidr": "<base+1>/30", "peer_ip": "<base+2>", "mtu", "shared_key", "forward_ports", "peer_panel", "user_listen_ip", "min_links": 2, "max_links": 0, "per_link": 8, "bind_local_ip"` (`install.sh:1897-1907, 2128-2138`). کلیدهایی که نصب‌کننده هرگز نمی‌نویسد: `tuning` (جز منوی Tuning)، `drain_idle_sec`، `backend_addr` سفارشی.
- **لینک `hs2://`** = base64 از ۱۳ فیلد با `|`: ENDPOINT، DOMAIN، **SHARED (کلید آشکار)**، PANEL، CARRIER، UDP، TRANSPORT، DIRECTION، MTU، ENCAP، PROTO، UNIT، TUN_BASE (`install.sh:1694-1741`)؛ سازگاری عقب‌رو تا لینک ۹ فیلدی.
- **unit systemd:** `Restart=always`، `RestartSec=3`، `TimeoutStopSec=8`، `KillMode=mixed`، `LimitNOFILE=1048576`، `ExecStartPre=-/sbin/modprobe tun`، `ExecReload=kill -HUP`؛ عمداً بی `User=`/`CapabilityBoundingSet=` (tune به `CAP_SYS_ADMIN` نیاز دارد) (`install.sh:816-849`). نصب‌کننده drop-in نمی‌نویسد.
- **اثبات اتصال:** `verify_tunnel` تا ۶۰s (`HS2_VERIFY_SECS`) با **ping روی hs0** برای هر حامل دارای رابط (`install.sh:898-951`) — در l3mtcp یعنی کانال جانبی، نه مسیر پورت کاربر.
- **ارتقا/پشتیبان/بازگردانی/حذف:** sha256 fail-closed با عبور از کش CDN؛ نصب اتمی باینری؛ ارتقای پیاپی تونل‌ها با `verify_tunnel` هر کدام؛ پشتیبان خودکار پیش از هر تغییر (۱۰ تای آخر، شامل کلید و کلید خصوصی گواهی؛ drop-inها نه)؛ بازگردانی با «گسست تمیز» باینری؛ `tm_edit` با diff، `hs2 check`، `.prev` و بازگشت خودکار (`install.sh:359-458, 3071-3150, 3858-4191`).
- گواهی: certbot با `--deploy-hook "pkill -HUP -x hs2"` و `renew_before_expiry = 30 days`؛ DNS-01 دستی خودکار تمدید نمی‌شود. کلاینت گواهی را وارسی نمی‌کند (`InsecureSkipVerify: true`، `tlscarrier/carrier.go:191`).

### ۹.۸ آزمایشگاه و اعتبارسنجی

- `hs2-src/lab/`: `netem` (شبیه‌ساز L2 در فضای کاربر: rate، delay، jitter، queue، loss، پلیس‌گر هر جریان و هر مقصد، Gilbert-Elliott، allow/dropudp)، `netsim` (UDP درون‌فرایندی)، `probe` (دانلود/آپلود/echo/اتصال تازه)، `dglab`، `tcpload.py`، اسکریپت‌های `run.sh` (حامل‌های جریانی، **فقط direct**)، `dgtun.sh`، `encap.sh`، `cpuquota.sh`، `transport-probe.sh` (مسیر واقعی، فقط reverse)، `sweep.py`/`analyze.py`.
- **مشاهده‌ها:** ستون `link_churn` در `run.sh` عملاً همیشه صفر است (الگوی `reap:|rebuilt|link down` با لاگ امروز `mtcp: link %d down: …` نمی‌خواند؛ `run.sh:90`)؛ `run.sh` اتلاف انفجاری، jitter و پلیس‌گر مقصد را برای حامل‌های جریانی نمی‌دهد؛ rig آزمون بار Q6–Q8 در مخزن نیست و عددهای ۳۰۰ لینک، ری‌استارت و قطعی فقط در CHANGELOG ثبت‌اند؛ `probe` پس از نخستین echo گم‌شده متوقف می‌شود.
- `hs2-src/VALIDATION.md`: چک‌لیست میدانی V1–V14 و شناسهٔ انتشار `da621f5db2bb`. **باینری منتشرشده (`hs2-linux-amd64`) از `da621f5` ساخته شده و `git diff da621f5 0812bc9` فقط باینری، هش و `VALIDATION.md` را نشان می‌دهد؛ پس path:lineهای این سند دقیقاً رفتار میدانی‌اند.** بازتولیدپذیری بایت‌به‌بایت **نامطمئن**.

---

## ۱۰. تاریخچهٔ فازها و ایده‌های ردشده

### ۱۰.۱ خط زمانی

| دوره | تاریخ (۲۰۲۶) | محتوا |
|---|---|---|
| ۰ | ۰۹-۲۸ | بارگذاری دستی کد v2 |
| ۱ | ۰۹-۲۸ | P0 اصلاح HOL در L3 چندلینکی (`d82d19f`)؛ **P1 hs2 v3 = هستهٔ stream** (`765288a`) |
| ۲ | ۰۹-۲۸ | PR#1–#4: core + fec، حامل UDP، جهت reverse و اصلاح‌هایش |
| ۳ | ۰۹-۲۹ | P2 شکل‌دهی طول رکورد، P3 سخت‌سازی DPI، P4 حالت نصب tun (l3mtcp)، P5 سلامت لینک سه فاز |
| ۴ | ۰۹-۳۰ | P8 autopilot ۲..۳۲، P9 وضعیت/tune/گواهی، **P11 pool v2** |
| ۵ | ۰۹-۳۰..۱۰-۰۱ | P13 نسل دوم tun دیتاگرامی، Governor، اصلاح‌های میدانی icmp |
| ۶ | ۱۰-۰۲..۱۰-۰۳ | CHANGELOG: Track A/B، فازهای C، E، F، G، H، I |
| ۷ | ۱۰-۰۳..۱۰-۰۴ | **فاز Q** (۳۰۰ لینک، Q1–Q8) |
| ۸ | ۱۰-۰۵..۱۰-۰۷ | فازهای V، W، X، Y، CA، CA1b (همه دیتاگرامی) |

ثبت پایانی `0812bc9` (build `da621f5db2bb`)؛ نقطهٔ بازگشت اعلام‌شده `9b74b3c` (فاز W). **آخرین تغییر منطقی مسیر stream در `f9b668c` (Q8) است**؛ `l3_link.go`، `stream*.go`، `mtcp_link.go`، `wedge.go`، `shape.go` و `tlscarrier/` پس از آن تغییر نکرده‌اند (تأیید ۹۱ ردیف ۷۸). CHANGELOG فقط از ۲۰۲۶-۱۰-۰۲ شروع می‌شود؛ پیش از آن تاریخچه فقط در پیام ثبت‌هاست. **برخورد نام:** «Phase B/C/D/G/H» هم در پیام‌های نسل دوم دیتاگرامی و هم در CHANGELOG با معنای دیگر آمده؛ «phase 1/2/3» سه معنی دارد.

### ۱۰.۲ فازهای مرتبط با l3mtcp (خلاصه)

| فاز | تغییر | اثر ماندگار |
|---|---|---|
| P0 `d82d19f` | خوانندهٔ TUN همگام با مهلت ۵s و `hash % n` ⇒ صف کران‌دار و نویسندهٔ جدا، دورریز، keepalive ۲s، rendezvous، ادغام در یک نوشتن TLS | سازوکار کانال L3 امروز (`l3QueueLen=256`، `l3BatchBytes=16KiB`، `l3KeepaliveEvery=2s`) |
| P1 `765288a` (v3) | پایان TCP-in-TCP برای پورت‌های کاربر؛ هستهٔ stream؛ hs0 فقط کانال جانبی با ۶۰ms؛ `Pick` با رزرو زیر قفل؛ بستن جلسه با اولین خطای I/O؛ BBR، `NOTSENT_LOWAT=32KiB`، قاب smux ۱۶KiB | l3mtcp زیر throttle: ۳۴٫۸→۴۷٫۱ Mbit/s، تأخیر ۲۵۶→۲۰۳ms، اتصال تازه ۵۲۶→۳۴۳ms (`README.md:680-691`؛ احتمالاً پیش از autopilot سنجیده شده — **نامطمئن**) |
| PR#3/#4 | جهت reverse؛ نقش smux ثابت؛ شناسهٔ لینک یکنوا | `engine/stream_reverse.go` |
| P2 `870a1c5` | `shapedConn` واقعی (پیش‌تر `LengthSampler` ساخته می‌شد ولی به کار نمی‌رفت) | ۹ اندازهٔ رکورد، سربار ۰٫۴۴٪ |
| P3 `7ad9d16`/`2634c34` | stagger ۴۰–۱۶۰ms، keepalive smux ۴–۸s/۲۴s | DPI eval: تک‌جریان AUC ۰٫۵۲؛ تعداد اتصال ~۱٫۰ |
| P4 `c7d17ae` | حالت نصب «tun» (l3mtcp) با MTU ۱۳۲۰ | منشأ پیش‌فرض ۱۳۲۰ راه الف |
| P5 | `meteredConn`، loss با TCP_INFO، `kindCtrl` | `lossFrac=0.12`، `degradeStreak=3`، `controlInterval=3s` |
| P8 `5c14e9d` | autopilot و `kindPool` | `warmStartLinks=8` |
| P11 pool v2 | `wrBlocked`، serving/retiring، `kindStats`، شبیه‌ساز، سود اندازه‌گیری‌شده، ضد خزش، suspect ۱۲s، `drain_idle_sec` | ستون فقرات پول امروز |
| P14 `4ff443a` | انتخاب mtcp+tun یا tls+tun؛ «l3mtcp در عمل همان موتور mtcp است» | تمیز ۸۷٫۷، ۱٪ اتلاف ۸۹٫۳، ۵Mbit/اتصال ۲۶٫۲ (tls: ۴٫۴) |
| C/E/G | هویت منسجم «سرویس HTTPS گو»؛ پاسخ ۴۰۰ دقیق؛ صفحهٔ پوششی per-install | سمت سرور TLS |
| H | `max_links` سه‌حالته، `kindInfo` برای تبادل سقف | سقف به اندازهٔ سرور |
| I | `port_map`، `kindTCPPort/kindUDPPort`، `kindInfo` v2، دروازهٔ info | مسیریابی هر پورت |
| Q1–Q8 | سقف تا ۳۰۰، dial gate، warm، refill، wedge guard، MPTCP خاموش، تخلیهٔ مرحله‌ای، stuck، loss پهنای باند بالا، فشار حافظه، تخمین ظرفیت هر لینک (`f9b668c`) | قواعد سلامت امروز |

### ۱۰.۳ عددهای سنجیدهٔ کلیدی

| موضوع | عدد | منبع |
|---|---|---|
| `NOTSENT_LOWAT` جاروب | ۱۶–۳۲KiB بهترین؛ ≥۶۴ تأخیر بیشتر؛ خاموش ۳–۷× بدتر | `tlscarrier/tune_linux.go:12-18` |
| `wrBlocked` | مسیر نامحدود ۰٪؛ گلوگاه ۲MB/s ۹۹٫۸٪ | `3e9becf` |
| autopilot | ۲۰۰Mbit، policer ۱۰Mbit/جریان، ۲۴ جریان ⇒ ۵۲ Mbit/s | `5c14e9d` |
| ۳۰۰ لینک | ~۳۱s (دروازه)؛ آزمون بار ~۵۷s؛ ~۴۸٪ یک هسته؛ RSS ۴۰۰–۴۸۰MB | `CHANGELOG.md:745-746, 805-821` |
| ری‌استارت ایران زیر بار | ۱۶٫۳s (قبلاً ۷۱) | `CHANGELOG.md:812-813` |
| قطعی ۴۰s | سرویس عادی ۱۶–۲۰s پس از پایان (قبلاً ۶۱)؛ اولین لینک ~۳s | `CHANGELOG.md:813`؛ `89d52cb` |
| refill hold | ۲۴۰۰ اتصال روی یک لینک ⇒ ≤۲۴؛ ۷۸۱ ⇒ ۹۶ | `CHANGELOG.md:763-765` |
| MPTCP روشن/خاموش | echo p99 ۸s ⇒ ۰٫۵s | `CHANGELOG.md:827-832` |
| کاربران روی degraded تا ۵ دقیقه | p90 ۱٫۶–۲٫۷s در برابر ۰٫۲–۰٫۴ | `CHANGELOG.md:841-843` |
| stuck (Q7) | ۹ از ۱۰ لینک گیر در ۱۰٫۶s؛ echo ۷۶٪ ⇒ ۱۰۰٪ | `CHANGELOG.md:912-950` |
| loss (Q8) | پایدار: ۲۸ حکم/۱۳۱ قطع ⇒ ۱/۲؛ فشرده‌سازی ۱۹۰→۶۰Mbit: ۳۳ stuck/۳۹ loss/۱۷۳۸ قطع ⇒ ۱/۱/۹۹ | `CHANGELOG.md:1040-1055` |
| ۱۰٬۰۰۰ اتصال باز | ~۰٫۹GB RSS؛ ۷۴٪ یک هسته در ~۱۹۰Mbit/s | `CHANGELOG.md:1091-1093` |
| L3 failover | ۲۴–۳۰s ⇒ ~۱۲s | `99d4583`؛ `README.md:234-235` |

### ۱۰.۴ ایده‌های امتحان‌شده و ردشده

**هستهٔ stream و l3mtcp:**

| # | ایده | چرا کنار رفت | وضعیت |
|---|---|---|---|
| S1 | بستهٔ IP روی TLS (TCP-in-TCP) برای l3mtcp/tls | صف چندثانیه‌ای زیر بار | hs0 فقط کانال جانبی؛ «For bulk traffic over a routed tun, use tun over udp/icmp» (`README.md:658-664`) |
| S2 | خوانندهٔ TUN همگام با مهلت ۵s | یک لینک کند همه را می‌خواباند | صف هر لینک + دورریز |
| S3 | `hash % n` | مرگ یک لینک همه را جابه‌جا می‌کرد | rendezvous |
| S4 | `NOTSENT_LOWAT` ۱۲۸KiB | جاروب | ۳۲KiB |
| S5 | پول ثابت ۴، سپس ۸–۱۶ | — | autopilot |
| S6 | **کم کردن تعداد لینک برای پنهان شدن از DPI** | «a lone user needs ~8 links to beat per-connection shaping»؛ تعداد اتصال قوی‌ترین نشانه ولی «kept high on purpose for throughput» | **رد؛ تصمیم صاحب پروژه** (`230fe8d`) |
| S7 | stagger ۱۲۰–۴۸۰ms | خودش نشانه شد (AUC ۰٫۹۳→۱٫۰۰) | ۴۰–۱۶۰ms |
| S8 | keepalive ثابت ۵s | ضرباهنگ یکنواخت | ۴–۸s تصادفی |
| S9 | صفحهٔ nginx + `Server: nginx` | امضای honeypot؛ تناقض با TLS گو | صفحهٔ خنثی per-install |
| S10 | بستن بی‌صدای HTTP ساده | امضای اصلی کاوش فعال | پاسخ ۴۰۰ گو |
| S11 | peek پنج‌بایتی با `prefixConn` | RST به‌جای FIN و `TCP_INFO` تاریک | حذف در E |
| S13 | پنهان کردن اثرانگشت TLS گو (Reality یا nginx واقعی) | با تک‌باینری ایستا و احراز وابسته به کانال جور نیست | عمداً انجام نشد (`CHANGELOG.md:150-156`) |
| S14 | سقف کاوش‌سیل هر اتصال | — | به تعویق (باز) |
| S15 | لایهٔ دوم AEAD داخل TLS | فقط هزینهٔ CPU | لایه‌گذاری نمی‌شود (`BUILD.md:179-180`) |

**پول، autopilot و reverse:**

| # | ایده | چرا کنار رفت | وضعیت |
|---|---|---|---|
| P1 | اندازه فقط با تعداد کاربر | throttle هر اتصال را نمی‌دید | کف + پروب |
| P2 | نگه‌داشتن پروبی که فقط فشار را کم کرد | ۸→۱۰ با افت ۰٫۹ Mbit/s نگه داشته شد | سود اندازه‌گیری‌شده ≥۵٪ |
| P3 | ریست عقب‌نشینی با یک انفجار | جغجغه روی مسیر پر | فقط تقاضای پایدار |
| P4 | پذیرفتن سود خوش‌شانس روی مسیر پرنویز | خزش | CONFIRM ۶۰s + سقف مسیر |
| P5 | تخمین ظرفیت از میانهٔ **نمونه‌ها** | پول روی ۴۸ قفل شد | بهترین هر **لینک**، میانه (`f9b668c`) |
| P6 | سقف ثابت ۶۴ | سرور ۱GB تا ~۵۱۲MiB بافر | سقف به اندازهٔ RAM |
| P7 | تبادل سقف روی کانال آمار | برگردانده (`af3b6af`) | `kindInfo` جدا |
| P8 | خروجی معکوس جدیدترین لینک را برای کوچک کردن ببندد | اتصال زنده قطع می‌شد | خروجی هرگز برای کوچک کردن نمی‌بندد |
| P9 | لبه پیش از اندازه‌گیری هدف ۲ بفرستد | پول گرم خروجی بریده می‌شد | شروع گرم مشترک |
| P10 | پهن کردن `ip_local_port_range` | ناحیهٔ inbound پنل‌ها | متوقف و برگردانده |
| P11 | bind شنونده روی 0.0.0.0 | سرورهای چند-IP | برگردانده؛ `rp_filter=2` |
| P12 | رها کردن صف dial با یک شکست | ramp با ۱–۵٪ شکست گیر می‌کرد | ۳ شکست پیاپی |
| P13 | پیشاهنگ قطعی با backoff تا ۸s | اولین لینک تا ۸s دیر | ۱–۲s، connect ۲s |
| P14 | warm که پایین هم بیاورد و در crash-loop تازه شود | پول شبانهٔ ۲ برمی‌گشت | فقط بالا؛ پس از ۱ دقیقه |
| P15 | refill معکوس منتظر هدف لبه | ۴۲۸ از ۵۰۰ اتصال ۱۰s منتظر | `min(target, سقف خروجی)` + `refillStall` ۳s |
| P16 | ارسال تغییر هدف روی همهٔ لینک‌ها هم‌زمان | صدها پیام | ۲ لینک سریع، پخش ۱٫۵s |

**قاعده‌های سلامت:**

| # | ایده | چرا کنار رفت | وضعیت |
|---|---|---|---|
| H1 | throughput در برابر همتاها برای degrade | مثبت کاذب روی کاربران بیکار | loss + آستانهٔ فعالیت |
| H2 | ping کنترل لرزان | هشدار کاذب loss دانلود | ۳s ثابت روی لینک پرکار |
| H3 | پایان کانال کنترل با یک timeout نوشتن | loss کور می‌شد | ادامه با ≤۴ ping معلق |
| H4 | برش‌های اولیهٔ stuck | ۲۱۰، ۵۵، ۲۷ لینک در فشرده‌سازی | شاهد + پنجرهٔ بهبود |
| H5 | «دو برابر میانهٔ پرکارها» برای loss | پراتلاف‌ها میان throttleشده‌ها پنهان | مقایسه با pressedها |
| H6 | مخرج `bytes/1400` | ۱٫۱–۱۰× بیش‌برآورد | شمارش segment |
| H7 | loss روی تیک ۲s از آخرین pong | تابع لرزش pong بود | پنجرهٔ بین دو pong |
| H8 | تخلیهٔ همهٔ نامزدها | مسیر پراتلاف ۳۰۰ لینک را یک‌جا تخلیه کرد | ≤ `drainHeadroom` + رأی «مسیر پراتلاف» |
| H9 | شمردن ۲–۶s، سنگین‌ها و بی‌پاسخ‌ها به‌عنوان «کند» | دو لینک پراتلاف شبانه قواعد را خاموش کردند | فقط منتظرهای سبک |
| H10 | «بیش از یک‌سوم پرکارها منتظر» | اقلیت stuck به‌جای مسیر کند | «منتظرها از سریع‌ها بیشتر» |
| H11/H12 | مدرک «مسیر کار می‌کند» = انتظار <۳s / پاسخی که بعداً **رسید** | ۵۵ لینک stuck شد / پاسخ پیش از قطعی رفته | RTT آخر <۲s / ping که بعداً **فرستاده** شد |
| H13/H14 | پنجرهٔ بهبود ثابت ۳۰s / از اولین تا آخرین تیک کند | لینکی ۶۵s بعد برگشت و ۴۰ اتصال برید / ۱۳ لحظهٔ کوتاه قاعده را ۲۰ دقیقه خاموش کرد | ۳۰s–۲m به اندازهٔ کندی |
| H15 | کف جابه‌جایی ۴KB/تیک | throttle ۷KB/تیک فرار می‌کرد | `stuckMoveFloor`=12KiB |
| H16 | بستن همهٔ کاربران degraded در ۴۵s | ۶۰–۹۰ اتصال فعال هر لینک | مرحله‌ای |
| H17 | نگه‌داشتن کاربران تا ۵ دقیقه | p90 ۱٫۶–۲٫۷s | ۹۰s (**تصمیم صاحب پروژه**) |
| H18 | معیار flowing برای بستن در تخلیه | SSH تایپی و بازی بریده می‌شد | `idleStreams` بایت‌محور |
| H19 | لینک draining در سقف max | ۱۶ از ۱۶ degraded ⇒ ۲ serving | خارج از max؛ کل ≤ max + یک‌هشتم |
| H20 | بستن پیاپی اتصال‌های لینک گیر (FIN ۳۰s) | کاربر دوم ۳۰s معطل | هر بستن goroutine خودش |
| H21 | نگهبان فقط با بافر پر | زیر فشار حافظه ۱۱ لینک سالم تخلیه و ۴۴ کاربر قطع | squeeze + توقف حکم‌ها (`f9b668c`) |
| H22 | MPTCP پیش‌فرض گو | `notsent_lowat` نادیده؛ p99 ۸s | خاموش (**تصمیم صاحب پروژه**؛ `go.mod`) |
| H23 | پایان جریان آمار = خروجی قدیمی | لینک در حال مرگ «older hs2» می‌گفت | اگر به kindInfo جواب داده، گذراست |
| — | تشخیص مرگ فقط با keepalive smux | معطلی ≥۱۵s | `watchConn` |
| — | رهاسازی L3 پس از ۲۴–۳۰s | — | سکوت ۱۲ ثانیه‌ای نشست |
| — | نگهبان بی `starveCalls` | خوانندهٔ آهسته توقف را ۲۳s+ پنهان کرد | <۲۰۴۸ Read = پارک |
| — | سقف refill که با زمان دو برابر شود | ۱۹۲ در برابر ۹۶ | سهم منصفانهٔ ثابت |

**دیتاگرامی (روی l3mtcp اثر ندارد؛ D1–D25 در نقشهٔ ۱۲ §۵-د):** از جمله D1 «تقاضای محدود به pacing» (برگردانده؛ محدودیت واقعی rwnd TCP درون‌تونلی بود)، D3 Governor نسخهٔ ۱، D4 بازچین ۴۰ms (−۱۰٪)، D6 `icmp_echo_ignore_all` سراسری، D7 سوکت خام متصل (DoS با یک ICMP جعلی)، D8 یک سوکت خام برای هر حامل، D11 فقط rendezvous برای dgtun (۴۰٪ بازارسال میدانی)، D16 صف عمیق‌تر زیر اشباع («moved the number by nothing»)، D18 سکوت کامل CA1، D19 CA3، D20 CA4، D25 سرآیند IP برای gre/ipip/ipx.

**نصب‌کننده و عملیات:** I1 U4 rekey (رها شد «too complex»)، I2 B2 چند IP برای هر تونل (به تعویق، درخواست نگه‌دارنده)، I3 `trap … RETURN`، I4 «Renewal set» برای DNS-01، I5 ETag خام؛ همچنین `CapabilityBoundingSet` (tune را می‌شکست)، `pkill -x hs2` (تونل‌های دیگر را می‌کشت)، «ready» بر اساس سرویس در حال اجرا، فایل sysctl ایستا.

### ۱۰.۵ پسرفت‌های مرتبط با l3mtcp و اصلاحشان

| پسرفت | اصلاح |
|---|---|
| `TCP_INFO` سمت سرور TLS تاریک (`Carrier.TCPConn()` فقط یک لایه باز می‌کرد) ⇒ loss در خروجی مستقیم/لبهٔ معکوس هرگز شلیک نمی‌کرد | `NetConn()` و باز کردن کل زنجیره (`CHANGELOG.md:165-172`) |
| RST به‌جای FIN برای کاوش‌ها | حذف peek (E) |
| ping لرزان هشدار loss دانلود را برگرداند | ۳s ثابت (`4523694`) |
| خروجی معکوس لینک با کاربر را می‌بست | `aab38f9` |
| ساخت‌های stuck پس از هر فشرده‌سازی ۴–۸ لینک loss تخلیه کردند | `6d615c0` |
| شمردن هر لینک >۲s به‌عنوان «کند» قواعد را خاموش می‌کرد | `3abecc5` |
| لینک wedge شده توسط کاربران خودش ۱٫۷s پس از نگهبان stuck گرفته شد | `sessGuard.parkedAt` (`14bf88b`) |
| بافر ping بازاستفاده‌شده pingهای صف را بازنویسی می‌کرد | بافر جدا (`93e6a9b`) |
| ۲۷ لینک بلافاصله پس از فشرده‌سازی stuck شدند | `49337f4`، `217a5a8` |
| معیار flowing کاربران فعال را می‌برید | `cfd1a2c` |
| لینک در حال مرگ کند = «older hs2» | `8271d8f` |

### ۱۰.۶ مسائل باز اعلام‌شده (مرتبط با l3mtcp)

- **O1** فشرده‌سازی از صف کم‌عمق (۲۰ms): ۱۷۳۰/۱۷۵۰ قطع در برابر ۱۵۳۷ main؛ V5 باز (`CHANGELOG.md:931-936`؛ `VALIDATION.md:111-119`).
- **O2** در فشرده‌سازی‌ای که دور می‌ریزد، سبک‌های پاسخ‌دهنده از سنگین‌ها بیشترند؛ سنگین‌ها یکی‌یکی قضاوت می‌شوند («judged but not changed»؛ `engine/stuck.go:57-60`).
- **O3** هجوم ناگهانی روی پول از قبل بالا (~۲۳۵ اتصال روی ۸ لینک)؛ **O4** ۱۰٬۰۰۰ اتصال ≈۰٫۹GB؛ **O5** انفجار >۴۰ اتصال/دقیقه/لینک فشار را پنهان می‌کند (۶–۱۰٪ روی لینک throttled)؛ **O6** پروب معکوس سقف خروجی را نمی‌داند (`CHANGELOG.md:1086-1097`).
- **O7** wedge سمت خروجی از لبه دیده نمی‌شود؛ **O8** کانال L3 مسیر انبوه نیست؛ **O9** اثرانگشت TLS همان گو است؛ **O10** سقف کاوش‌سیل؛ **O11** پشتیبان‌ها کلید خصوصی دارند؛ **O12** سقف ۳۲ روی سرور ۱GB در میدان آزموده نشده؛ **O13** V1–V8 هنوز چک‌لیست است.
- O14–O23 دیتاگرامی یا نصب‌کننده‌اند (نقشهٔ ۱۲ §۷).

**تصمیم‌های صریح صاحب پروژه:** تعداد زیاد لینک با وجود نشانهٔ DPI (S6)؛ سقف ۹۰s برای کاربران لینک degraded (H17)؛ خاموشی MPTCP (H22)؛ تعویق B2 و رها کردن U4.

---

## ۱۱. «از قبل وجود دارد»: فهرست جامع دسته‌بندی‌شده

> هدف این بخش جلوگیری از دوباره‌کاری است. هر بند یعنی «این سازوکار همین حالا در کد هست»؛ جزئیات و path:line در بخش‌های قبل آمده و این‌جا فقط نشانی کوتاه می‌آید. برچسب **[dg]** یعنی فقط در dgtun/udpcarrier هست و به l3mtcp سیم‌کشی نشده.

### ۱۱.۱ معماری کلی

1. حذف TCP-in-TCP برای پورت‌های کاربر: اتصال TCP کاربر روی لبه تمام می‌شود و هر اتصال یک جریان smux است؛ خروجی خودش با `DialTimeout` ۵s به پنل وصل می‌شود (`engine/stream.go:18-31`؛ `engine/stream_kharej.go:199-244`).
2. پول چندلینکی TLS با کانال جانبی hs0 برای l3mtcp (`runStream(true,0)`، `cmd/hs2/main.go:405-424`).
3. جدایی نقش از جهت: لبه همیشه کلاینت smux و تصمیم‌گیر پول؛ `reverse` فقط شماره‌گیر TLS را عوض می‌کند؛ همهٔ حامل‌های stream در هر دو جهت.
4. یک باینری ایستا (`CGO_ENABLED=0`)، چند سرویس؛ باینری میدانی = `HEAD` منطقی (`da621f5`).
5. موتور تک‌حاملی جدا برای `reality`/`noise`/`udp`/`auto` و موتور پول datagram (dgtun) جدا؛ autopilot و dial gate بین پول stream و dgPool مشترک‌اند.

### ۱۱.۲ کانال جانبی hs0 (L3)

6. صف کران‌دار ۲۵۶ و نویسندهٔ جدا برای هر لینک؛ خوانندهٔ TUN هرگز مسدود نمی‌شود (`engine/l3_link.go:53, 192-199, 334-357`).
7. دورریز بر اساس زمان ماندن ۶۰ms (سنجش در لحظهٔ برداشتن) علاوه بر سقف ۲۵۶ (`:56-62, 203-233`).
8. دسته‌سازی تا 16KiB به‌اضافهٔ یک بسته در یک `WriteRaw`؛ قاب ۷ بایتی `[ftype][len:3][pad:3]`.
9. rendezvous hashing (FNV-1a + `mix32(h^id)`)؛ مرگ یک لینک فقط جریان‌های خودش را جابه‌جا می‌کند (`:305-323, 399-438`؛ `TestPickMovesOnlyDeadLinksFlows`).
10. keepalive بیکاری با بار تصادفی ۰..۹۵ بایت: ۲s، یا ۸–۱۲s «آرام» با مذاکرهٔ `capL3Quiet` در `kindInfo` (`:67-79, 131-136`؛ `engine/peerinfo.go:55`).
11. رهاسازی کانال وقتی کل نشست ۱۲s ساکت است (`watchSession`، `:143-169`)؛ مهلت خواندن قاب ۳۰s.
12. مرگ فوری و قرینه با `markDead` (بستن جریان تا همتا منتظر نماند)؛ `sync.Pool` و `ReadFrameReuse`.
13. گزارش ۳۰ ثانیه‌ای دورریزها: `l3: dropped %d packets in 30s on the tun side channel …` (`:380-393`).
14. hs0 یک بار برای کل عمر فرایند؛ حذف رابط کهنهٔ هم‌نام؛ `txqueuelen 2000`؛ route `/32` همتا (`tun/tun_linux.go:100-130`).
15. آزمون‌های انتها‌به‌انتها با TLS واقعی در هر دو جهت و هم‌زیستی با پورت کاربر (`engine/tun_mode_test.go:157-237`).

### ۱۱.۳ لینک: TLS، احراز و استتار

16. TLS 1.3 واقعی با سرور `crypto/tls` و گواهی Let's Encrypt؛ کلاینت utls v1.8.0 با `HelloChrome_133` (X25519MLKEM768، GREASE، GREASE ECH، ALPS، brotli).
17. احراز دوطرفهٔ متصل به نشست با EKM (`EXPORTER-hs2-channel-binding-v2`)؛ کلاینت پیش از اثبات سرور ساکت؛ ضد MITM بی‌وارسی زنجیره؛ ساعت ±۲ سطل دقیقه‌ای؛ حافظهٔ ضد replay FIFO با O(1) (۵m / ۱۶۳۸۴) (`tlscarrier/auth.go`؛ `tlscarrier/replaymem.go`).
18. پرکنندهٔ تصادفی رکورد احراز (۲۸۰–۷۲۰) و اثبات (۱۲۰–۴۸۰) به اندازهٔ درخواست/پاسخ HTTP.
19. «یک هویت منسجم» برای کاوشگر: پاسخ دقیق `400` Go برای HTTP خام، FIN/RST مثل Go (آزمون تفاضلی)، خواندن «یک رکورد» نه بایت ثابت، ارسال TLS کاملِ احرازنشده به پشتیبان HTTP واقعی؛ `firstReadTimeout` ۳۰s.
20. صفحهٔ پوششی per-install با `cover_seed` (بی JS، ETag نمک‌زده، `Last-Modified` نسبی، 404 برای غیر `/`، بی `Server`)؛ `backend_addr` برای سایت واقعی.
21. تعویض داغ گواهی (SIGHUP + پایش mtime دقیقه‌ای)، هشدار انقضا، تحلیل تمدید certbot در doctor.
22. شکل‌دهی طول رکورد (`shapedConn` + `LengthSampler`، ۹ اندازه ۴۰..۱۴۰۰، میانگین ≈۹۹۱، padding واقعی) روی همهٔ حامل‌های stream.
23. keepalive smux تصادفی ۴–۸s برای هر نشست؛ stagger دست‌دهی ۴۰–۱۶۰ms با ≤۸ هم‌زمان.
24. `TCPConn()` کل زنجیرهٔ پوشش‌ها را باز می‌کند تا `TCP_INFO` تاریک نشود؛ `bind_local_ip` بی بازگشت بی‌صدا.
25. core (برای noise/udp): Noise IKpsk2، `firstMAC` پیش از Noise، پنجرهٔ replay بیت‌نقشه‌ای، `Exporter`/`Binding`، سطل‌های پرکردن، keepalive با jitter.
26. ابزار ارزیابی DPI (`obfs/dpi_eval*.py`).

### ۱۱.۴ smux و سوکت

27. smux v2 با قاب 16KiB، پنجرهٔ جریان 2MiB، سطل نشست 8MiB؛ قابل تنظیم با `HS2_TUNE_SMUX_*` برای آزمایشگاه.
28. `meteredConn` (بایت‌شمار + زمان مسدودی Write >۱ms) و `watchConn` (بستن نشست با اولین خطای I/O با دلیل خوانا) در هر دو سمت (`engine/stream.go:74-202`؛ `engine/health.go:170-196`).
29. سوکت لینک: BBR (یا cubic)، `NOTSENT_LOWAT=32KiB`، `USER_TIMEOUT=20s`، `NODELAY`، keepalive ۳s کلاینت؛ شنونده با `SO_REUSEADDR` و بی MPTCP.
30. `relayStream` با بافر 32KiB و `relayDieGrace` ۵s؛ UDP کاربر با صف و نویسندهٔ جدا برای هر جریان (۲۵۶ / 512KiB، بیکاری ۲m).

### ۱۱.۵ پول، انتخاب و dial

31. `Pick`/`pickKey`: بی‌فشار اول، کمترین `flowing+picks`، کمترین users، تصادفی؛ کلاهک انفجار `max(1, perLink/2)` در ۳ نمونه؛ سه لایه (serving → retiring سالم → هر زنده)؛ رزرو زیر قفل.
32. دروازهٔ info پیش از اولین کاربر و fallback به `exitInfo`.
33. سنجاق اتصال به لینک (عمداً بی‌مهاجرت).
34. retiring به‌جای بستن؛ un-retire پیش از dial؛ ترتیب قربانی «زودتر خالی‌شدن»؛ بستن ۲..۸ در تیک با لرزش؛ `drain_idle` ۳۱۰s و `retireForce` ۲۰m.
35. dial gate سراسری (۸ هم‌زمان، ۴۰–۱۶۰ms، `acquireIf` برای dialهای هنوز لازم)؛ epoch با تحمل ۲ شکست؛ گام `ceil(T/4)`؛ صف آغاز `min(warm, ceil(warm/4)+8)`.
36. پرکردن فقط تا هدف (نه بر اساس تعداد کاربر).
37. شروع گرم (۸ یا اندازهٔ پیش از ری‌استارت ≤۱۵m، فقط افزایشی، نوشتن پس از ۱m، ضد crash-loop).
38. refill hold پس از شروع/قطعی کامل با سقف سهم منصفانهٔ ثابت (≤۱۰s، `refillStall` ۳s، `refillRearm` ۱m).
39. سقف پول به اندازهٔ سخت‌افزار (۴۸MB/لینک، ≤۳۰۰، ≤۱۲۸ زیر ۴ هسته، کف پروفایل، آگاه از cgroup)؛ سقف ۸ برای icmp؛ ۳۲ برای پیکربندی قدیمی؛ `min_links` سقف را بالا می‌برد.
40. تبادل سقف دو سمت با `kindInfo` و نمایش «capped at N by the Kharej server».
41. هشدار لینک‌های کوتاه‌عمر («سه لینک پیاپی که ظرف ۲۰s می‌میرند»).

### ۱۱.۶ autopilot

42. کنترلر خالص و قطعی، با شبیه‌ساز جریان‌سطح و ده‌ها آزمون سناریو (`engine/autopilot_sim_test.go`).
43. کف از جریان‌های **واقعاً فعال** (`fl5`، EWMA ۱۰s، ۲KiB/s، تازگی ۶s، یا ۲۵۶B/s پیوسته)؛ اتصال‌های بیکار پنل شمرده نمی‌شوند.
44. رشد فقط با پروب آزمون‌شده؛ تفکیک افزایشی از جانشینی با آمار (z=۲٫۵، نصف ترافیک تازه، ۵٪ پایه)؛ شکست زودهنگام؛ relieved؛ spares.
45. گام ۲۵٪/۵۰٪ با سقف ۳۲/۶۴؛ مهلت مسلح‌شدن متناسب با گام؛ `spare(p)` مقیاس‌پذیر تا صدها لینک.
46. عقب‌نشینی نمایی ۳۰s→۸m با ±۲۰٪؛ abort ۶۰s→۸m؛ بازنشانی با رشد **پایدار** تقاضا یا ۳۰m.
47. ضد خزش: سقف مسیر `apCeil` (۱h) و CONFIRM (۶۰s).
48. کوچک‌سازی تقاضامحور با H، گام نصفِ فاصله، dwell ۶۰s؛ RESTORE فوری + hold ۱۰m→۲h که با افت تقاضا باطل می‌شود.
49. تخمین ظرفیت **هر لینک** (بهترین پایدار ۳۰m، میانه، ≥۶ لینک) فقط برای کوچک‌سازی.
50. فشار دوجهته: آپلود محلی (`wrBlocked` + chrono) و دانلود از رکورد `kindStats` خروجی؛ استثنای rwnd و اشارهٔ `tcp_rmem`؛ ۲ از ۳.
51. پشتیبانی کامل معکوس: `kindPool`، `ctlTarget`، born-spare، churn guard، `growable`.
52. نمایش زنده: phase، reason، `CapMbit`، `PeakMbit`، `NextProbeS`، `Pressed`، `Saturated`؛ note های `mtcp: pattern …`.

### ۱۱.۷ سلامت و تشخیص خرابی

53. suspect ۱۲s بی‌دریافت (برگشت‌پذیر).
54. کانال کنترل per-link: RTT، retrans خروجی (loss دانلود)، `ctrlWait` (سن قدیمی‌ترین پینگ)؛ آهنگ وابسته به بار؛ ≤۴ پینگ معلق.
55. قاعدهٔ loss دوجهته با شمارش segment، پنجرهٔ pong، نگه‌داشت streak در نمونهٔ آرام، مقایسه با نرخ pressedها، رأی «مسیر پراتلاف»، سقف `drainHeadroom`.
56. قاعدهٔ stuck با پینگ پشت ترافیک خود لینک، شاهد «رفت‌وبرگشتی که بعدتر شروع شده»، کف جابه‌جایی، استثنای wedge و suspect.
57. تشخیص مسیر کند/شلوغ (شمارشی + تورم RTT با پایهٔ ۱۰ دقیقه‌ای) و پنجرهٔ بهبود متناسب با طول کندی (۳۰s–۲m).
58. wedge guard بی syscall با آزادسازی هدفمند (RST) و `starveCalls`؛ حالت فشار حافظهٔ TCP (محلی و طرف مقابل) که guard را تهاجمی و حکم‌ها را خاموش می‌کند.
59. تخلیهٔ پله‌ای degraded (۴۵/۱۵/۹۰s)، بستن موازی، draining خارج از max (کل ≤ max + یک‌هشتم)، پس‌دادن جا در سقف؛ make-before-break.
60. `TCP_USER_TIMEOUT` و `watchConn` برای مرگ سریع نشست (به‌جای ۱۵s+ keepalive).

### ۱۱.۸ معکوس

61. پول slot خروجی که لبه هدایتش می‌کند: هرگز بستن خودسرانه برای کوچک‌سازی؛ بازنشستگی slot با پایان لینک؛ backoff ۰٫۵–۸s؛ redial سریع (۲۵۰–۵۰۰ms) برای لینک ≥۳۰s؛ پیشاهنگ قطعی با connect ۲s.
62. سقف پذیرش لبه `2×max+8` روی لینک‌های **زنده** و نگه‌داشت ۵s لینک ردشده.
63. pool-control روی ۲ لینک سریع (۲٫۴–۳٫۶s) و بقیه (۲۴–۳۶s)؛ پخش تغییر ≤۱٫۵s؛ تشخیص خروجی قدیمی (`growable=false`).
64. born-spare با `bornSpareGrace` ۳۰s، `retireAfterDrop` ۴s، churn guard (۳ دقیقه).
65. لاگ خروجی که دلیل رفتن لینک را می‌گوید (`retired — closed by the edge …` / `lost (…)` / `no data from the edge for 24s …`).

### ۱۱.۹ پایش و عملیات

66. فایل وضعیت زنده هر ۲s (اتمی، مسیر قطعی هم‌خوان با نصب‌کننده)؛ `hs2 status [--watch]`.
67. پایش CPU فرایند و میزبان (busy، softirq، steal، PSI، OutDiscards) با پسماند.
68. پایش `tcp_mem` و تغذیه به موتور.
69. `hs2 doctor` با ۱۵ بررسی فقط‌خواندنی؛ `hs2 check` با خط/ستون خطای JSON و بررسی IP محلی؛ `hs2 config` با فهرست سفید؛ `hs2 ports`؛ `hs2 tune`؛ `hs2 recommend-links --why`؛ `hs2 cleanup`؛ pprof فقط loopback.
70. تنظیم خودکار هسته در هر شروع (سه پروفایل، auto/manual/off، bbr→cubic، fq_codel→fq)؛ دست‌نزدن به `ip_local_port_range`؛ `GOMEMLIMIT` نصف RAM.
71. `burstLog` و لاگ‌های محدودشده برای صدها لینک.

### ۱۱.۱۰ نصب‌کننده

72. چند تونل هم‌زمان (سرویس، پیکربندی، رابط و `/30` جدا) با تشخیص برخورد نام و زیرشبکه.
73. لینک ۱۳ فیلدی `hs2://` با سازگاری عقب‌رو؛ `max_links: 0` خودکار در نصب تازه.
74. اعتبارسنجی هر پیکربندی با `hs2 check` و نصب اتمی؛ sha256 fail-closed با عبور از کش CDN.
75. ارتقای تک‌تونل/همه با تأیید و `verify_tunnel`؛ پشتیبان خودکار (۱۰ تا)؛ بازگردانی با «گسست تمیز»؛ ویرایش امن با diff و بازگشت.
76. صفحه‌های Tuning، Ports، Link pool، گواهی (سه روش، وضعیت تمدید)، مانیتور زنده و doctor از منو.
77. unit مقاوم (`Restart=always`، `RestartSec=3`، `ExecReload` با SIGHUP، `LimitNOFILE`).

### ۱۱.۱۱ فقط در dgtun/udpcarrier (به l3mtcp سیم‌کشی نشده)

78. **[dg]** TUN با offload (`IFF_VNET_HDR`، TSO/GRO، `splitTCP`) و `tunBatch` (`tun/tun_linux.go:73-298`؛ `tun/offload.go`؛ `engine/tunbatch.go`).
79. **[dg]** reorderer ۱۵ms هر حامل (`engine/reorder.go`).
80. **[dg]** جدول sticky با flowlet ۳۰۰ms (`engine/dgpool.go:549-556, 978-1043`).
81. **[dg]** صف منصفانهٔ DRR با خط سریع جریان تُنُک و دورریز چاق‌ترین (`engine/dgfq.go`).
82. **[dg]** اعلام صریح mute/hear به همتا، zombie-on-fresh-link، scout، `stallGate` ضد توقف VM.
83. **[dg]** شمارنده‌های دورریز به تفکیک علت و فیلدهای `tun_*` در وضعیت.
84. **[dg]** FEC تطبیقی، کنترل نرخ تأخیرمحور، pacer سه‌صفه، Governor پولی، قاعدهٔ stage برای CPU، `hostSaturated`.
85. **[dg]** encapهای udp/icmp/gre/ipip/ipx، ابهام ICMP، echoguard، سوکت مشترک، `recvmmsg/sendmmsg`، استتار CA.

### ۱۱.۱۲ آنچه **وجود ندارد** (با grep یا خواندن کد تأیید شده)

| ندارد | شاهد |
|---|---|
| حامل QUIC، WebSocket، HTTP/2، gRPC؛ domain fronting / CDN؛ ECH واقعی | grep: صفر (`quic` فقط در توضیح `obfs/shaper.go`) |
| فشرده‌سازی | grep `compress`/`zstd`: صفر |
| NAT، `ip_forward`، MSS clamp، مسیریابی سیاست‌محور برای hs0؛ فایروال | grep: صفر (`iptables`/`nft` فقط برای echoguard) |
| offload، دسته‌نویسی، بازچین، sticky، صف منصفانه در l3mtcp | فقط در `engine/dgpool.go:670-671` ساخته می‌شوند |
| آگاهی انتخاب لینک hs0 از سلامت/بار لینک | `l3Set.pick` فقط `Alive()` |
| بازگشایی کانال L3 یا کانال کنترل روی لینک زنده | `openL3` فقط در `OnLink` |
| اولویت hs0 یا جریان‌های کوچک در smux | heap با دو کلاس CTRL/DATA و FIFO |
| مهاجرت اتصال TCP کاربر بین لینک‌ها یا پخش یک اتصال روی چند لینک | سنجاق تا آخر عمر |
| ورودی RTT، loss، CPU یا حافظه در `pick` و autopilot | — |
| کنترل پذیرش یا سقف تعداد اتصال کاربر | `serveUserTCP` بی‌سقف |
| backoff نمایی dial در لبهٔ مستقیم | هر تیک دوباره صف |
| منطق سلامت لینک در خروجی مستقیم | `LinkManager` فقط در `RunIran` |
| شمارندهٔ دورریز hs0 به تفکیک علت و فیلد `tun_*` در وضعیت l3mtcp | — |
| بارگذاری زندهٔ پیکربندی (جز گواهی) | — |
| IPv6 در کپسول‌های خام | `ip4:` |
| مهلت جدا برای `OpenStream` و سرآیند جریان کاربر | — |
| لاگ برای شکست dial پنل در خروجی | `engine/stream_kharej.go:233-237` |
| رساندن نشانی واقعی کاربر به پنل (PROXY protocol) | grep: صفر |
| ازسرگیری نشست TLS (PSK) در کلاینت | کش نشست utls نیست |
| سقف پذیرش لینک در خروجی مستقیم | فقط در لبهٔ معکوس |
| پشتیبانی نیم‌بسته (half-close) در رله | `engine/wedge.go:315-344` |

---

## ۱۲. محدودیت‌ها و مشاهده‌ها (بدون طرح تغییر)

> «مشاهده» یعنی چیزی که در کد دیده شد؛ پیشنهاد تغییر نیست. مواردی که به رفتار هسته یا شبکه بستگی دارند **نامطمئن** علامت خورده‌اند.

### ۱۲.۱ hs0 / L3

1. **مرگ دائمی L3 با یک نوشتن کند:** هر خطای `WriteRaw` (از جمله ۵s انتظار برای نویسنده **یا پنجرهٔ همتا**) ⇒ `markDead` بی‌بازگشایی؛ سکوت ۱۲s نشست هم همین. قطعی کوتاه، wedge یا لینک کند ظرفیت hs0 را بی‌صدا کم می‌کند؛ در قطعی ۱۲–۲۰ ثانیه‌ای ممکن است همهٔ L3ها بمیرند در حالی که لینک‌ها زنده بمانند (`engine/l3_link.go:237-240, 143-169`؛ `engine/stream_iran.go:126-128`).
2. **بستهٔ صف L3 مرده شمرده نمی‌شود** (سنجیده: `drops=0`)؛ لاگ ۳۰ ثانیه‌ای کمتر از واقع می‌شمارد و علت‌ها («بی‌لینک»، «صف پر»، «۶۰ms») را جدا نمی‌کند.
3. ورودی L3 مرده تا ~۳۰s در `l3Set` می‌ماند (FIN پشت `sendLoop`)؛ برای `pick` بی‌ضرر، ولی یک goroutine گیرکرده برای هر لینک.
4. FIN کلاس CTRL ممکن است از حداکثر یک قاب DATA صف‌شدهٔ همان جریان جلو بزند؛ گیرنده قاب ناقص یا EOF می‌بیند و L3 خودش را می‌کشد (استنتاج؛ آزمون ندارد).
5. **TUN کور به سلامت پول:** جریان hs0 روی لینک degraded تا ۹۰s، روی suspect/stuck تا ۱۲–۱۴s یا مرگ لینک می‌ماند؛ خروجی هم مستقل انتخاب می‌کند.
6. **هر تغییر پول جریان‌های hs0 را جابه‌جا می‌کند:** افزودن لینک ≈1/(N+1) (سنجیده ۳۳٫۳/۱۹٫۷/۱۰٫۸/۲٫۷٪ برای N=۲/۴/۸/۳۲)، بستن retiring و born-spare معکوس هم؛ بازچین و sticky نیست ⇒ خطر بی‌ترتیبی. هیچ آزمونی ترتیب چندلینکی را نمی‌سنجد.
7. رفت و برگشت یک جریان معمولاً روی دو لینک متفاوت است (شناسه‌های تصادفی مستقل)؛ ICMP و IPv6 بین دو میزبان در هر جهت روی **یک** لینک (هش درشت‌دانه)؛ قطعه‌های غیراول IPv4 بایت بار را به‌جای پورت هش می‌کنند.
8. سقف ۶۰ms فقط صف L3 را می‌سنجد، نه heap smux، سوکت، شبکه یا سطل گیرنده؛ دستهٔ L3 پشت حداکثر N قاب 16KiB جریان‌های حجیم همان لینک می‌ایستد.
9. بی offload و دسته‌نویسی: یک `read()` و یک `write()` برای هر بسته؛ `dev.Write` هم‌گام و بی‌بررسی خطا؛ حلقهٔ خطای خواندن TUN بی‌مکث (`engine/l3_link.go:344-350, 373-375`). اندازهٔ این گلوگاه برای l3mtcp سنجیده نشده.
10. TCP-in-TCP برای هر ترافیک حجیمی که اپراتور روی hs0 مسیریابی کند (همان چیزی که v3 برای پورت‌های کاربر کنار گذاشت). **این‌که کاربر hs0 را برای بار حجیم به کار می‌برد یا فقط ping، از کد معلوم نیست.**
11. ری‌استارت خارج hs0 خارج را از نو می‌سازد و مسیرهای دستی روی آن از بین می‌روند (استنتاج از رفتار tun غیرماندگار).
12. هیچ MSS clamp نیست؛ MTU hs0 به MTU مسیر بیرونی وابسته نیست (بسته روی جریان بایتی می‌رود)؛ حافظهٔ بدترین حالت `256×(MTU+128)` برای هر لینک (≈۱۱۱MB در ۳۰۰ لینک با MTU ۱۳۲۰؛ محاسبه).
13. رؤیت‌پذیری کم: مرگ L3 لاگ ندارد؛ تعداد لینک L3 در وضعیت نیست؛ `checkTun` فقط بالا بودن و نشانی را می‌سنجد؛ اثبات اتصال نصب‌کننده فقط ping روی hs0 است.
14. کد مرده: `l3Set.removeDead`/`count`/`closeAll`، `l3DeadAfter`، `newL3Link` روی حامل TLS خام؛ `remove` دم برش را nil نمی‌کند.

### ۱۲.۲ پول و انتخاب

15. لینک تازه‌مرده از ≈۶s تا stuck/suspect «سبک‌ترین» است و کاربر تازه جذب می‌کند؛ فقط کلاهک انفجار مهار می‌کند.
16. اتصال تازه روی لینکی که نویسنده‌اش گیر است تا ۳۰s در `OpenStream` (SYN) می‌ماند و سرآیند جریان مهلت ندارد؛ بدترین حالت با `pickWait` ≈۶s و ۳ تلاش بیش از یک دقیقه است (محاسبه).
17. لایهٔ ۲ `pickLocked` در قطعی کامل کاربر را روی لینک suspect می‌گذارد؛ refill فقط با `alive==0` شروع می‌شود.
18. suspect جای max را می‌گیرد (`dialRoomLocked`)؛ در سقف، جایگزین تا مرگ TCP ساخته نمی‌شود؛ `heal` retiring را بی‌سنجش suspect برمی‌گرداند.
19. در معکوس لینک فقط suspect جایگزین نمی‌گیرد تا خروجی مرگش را بفهمد (≈۲۰–۴۸s)؛ `ctlTarget` فقط draining را می‌شمارد.
20. `Stats()` لینک suspect را serving می‌شمارد ولی `countsLocked` نه؛ شمار suspect/degraded/draining در `PoolStats` نیست.
21. لاگ suspect تاشونده نیست؛ بازگشت از suspect لاگ ندارد؛ عدد «up» در لاگ «dials failing» در واقع serving است؛ چند توضیح کهنه (`engine/linkmanager.go:695-697, 1817`).
22. فیلدهای مرده/تشخیصی: `managedLink.dead`، `mtcpLink.dead`، `goodput`، `lossFrac`، `linkMeter.stalls`.
23. قفل سراسری: هر `Pick` قفل نوشتن `m.mu` را می‌گیرد و `sampleHealth` کل حلقه را زیر همان قفل اجرا می‌کند (اثر در صدها لینک **نامطمئن**).
24. dial مستقیم بی backoff/scout؛ `DialFrom` ctx نمی‌گیرد؛ مسیری که TCP را پس از چند KB می‌کشد ⇒ لینک‌های کوتاه‌عمر پیوسته.
25. خروجی مستقیم سقف پذیرش ندارد؛ لاگ‌های «link up/down from» خروجی مستقیم تجمیع نمی‌شوند.
26. شکست dial پنل بی‌صدا است (لبه پیش از دانستن نتیجه داده می‌فرستد)؛ نشانی واقعی کاربر به پنل نمی‌رسد؛ half-close پشتیبانی نمی‌شود.
27. سقف توان هر جریان = پنجره/RTT: با 2MiB حدود ۱۶۰Mbit/s در RTT ۱۰۰ms و ۵۵Mbit/s در ۳۰۰ms؛ سطل 8MiB یعنی حداکثر ۴ جریان کاملاً پر روی یک لینک (محاسبه). UPD کلاس DATA دارد و پشت قاب‌های آپلود می‌ایستد.

### ۱۲.۳ تشخیص خرابی

28. لینکی که پس از احراز هیچ قاب smuxی نگیرد هرگز suspect نمی‌شود (`rxSeen=false`) و پس از `infoTimeout` قابل pick است.
29. بی‌شاهد (پول ۱–۲ لینکه) stuck کار نمی‌کند.
30. loss و stuck سقف تخلیهٔ جدا دارند ⇒ تا ≈۲× headroom در یک تیک (قصد طراحی **نامطمئن**). degraded برگشت‌ناپذیر است.
31. شکاف‌ها: خوانندهٔ کند ولی زنده (F13)، پراتلاف کم‌حجم (F9)، تأخیر ۲–۵s (F10)، اشباع CPU (F24)، حافظهٔ پردازه (F25)، توقف VM (F31) — §۷.۴.
32. کانال کنترل پس از خطای غیر timeout و `openPoolCtl` پس از هر خطای نوشتن هرگز باز نمی‌شوند.
33. keepalive سه‌ثانیه‌ای فقط سمت dialer و فقط `TCP_KEEPIDLE` (فاصله ۱۵s و تعداد ۹ پیش‌فرض Go)؛ سمت پذیرنده پیش‌فرض Go. اثر عملی **نامطمئن**؛ `USER_TIMEOUT` تعیین‌کننده است. کامنت «تشخیص سیاه‌چاله در هسته» (`tlscarrier/carrier.go:183-189`) قصد کد را می‌گوید، نه اثر سنجیده.
34. `Carrier.writeTimeout` ۵s روی مسیر smux اعمال نمی‌شود (`RawConn` آن را دور می‌زند)؛ نوشتن smux مهلت ندارد.
35. آهنگ عملی ping روی لینک مرده ≈۶–۹s (pong تا ۶s مسدود، نوشتن ping تا ۳s؛ استنتاج)؛ روی حکم stuck اثر ندارد.
36. نگهبان گیر UDP و L3 را پوشش نمی‌دهد؛ wedge سمت خروجی از لبه دیده نمی‌شود؛ تا ۴ جریان منتظر dial کند پنل می‌توانند لینک را تا ۵s نگه دارند.

### ۱۲.۴ autopilot

37. کور به تأخیر و loss؛ معیار نگه‌داشتن لینک فقط «افزایش throughput» است.
38. بار hs0 برای کف نامرئی است؛ `U = fl60+2` می‌تواند سقف پایینی بگذارد اگر بار اصلی از hs0 بگذرد (اثر عملی **نامطمئن**).
39. رشد به `S ≥ T` شرط شده؛ هر کسری serving رشد را می‌بندد.
40. تأخیر ذاتی رشد: سریع‌ترین موفقیت ≈۱۶s پس از شروع پروب؛ رد نادرست تا ۸ دقیقه رشد را عقب می‌اندازد و نرخ آن آزموده نشده.
41. abort بی‌لرزش؛ `belowSince` در طول پروب یخ می‌زند.
42. `needBW` به میانهٔ ظرفیت لینک‌ها تکیه دارد و تا ≥۶ لینک متمایز pressed نشده باشند صفر است.
43. فاصلهٔ T تا بستن فیزیکی چند دقیقه است.
44. `pressed` به `NOTSENT_LOWAT` وابسته است؛ `HS2_TUNE_NOTSENT` آستانه را بی‌صدا جابه‌جا می‌کند.
45. هر تغییر در `autopilot.go` dgtun را هم عوض می‌کند.

### ۱۲.۵ TLS و استتار (برجسته‌ها از م1–م23 نقشهٔ ۰۷)

46. طول رکورد هیچ‌وقت از ۱۴۰۰ (+۲۲) بیشتر نمی‌شود، در حالی که HTTPS حجیم معمولاً رکورد 16KiB دارد؛ طیف دقیقاً ۹ مقدار ثابت (۶۲…۱۴۲۲) در همهٔ نصب‌ها و هر دو جهت (م1، م2). استفادهٔ DPI واقعی **نامطمئن**.
47. هر رکورد یک `Write` و یک syscall (~۱۶× حالت 16KiB)؛ نوشتن‌های کوچک (NOP، ping، کلید SSH) تا ≈۹۹۱ بایت پد می‌شوند (م3؛ اثر CPU **نامطمئن**).
48. کاوشگر TLS 1.2 بی EMS پس از دست‌دهی بی‌پاسخ بسته می‌شود (م4)؛ ناهمخوانی مهلت ۳۰s/۱۰s (م5)؛ ALPN فقط `http/1.1` (م6)؛ نبود ازسرگیری نشست — صدها دست‌دهی کامل (م7)؛ پیر شدن `HelloChrome_133` (م8).
49. اختلاف ساعت با پیام گمراه‌کنندهٔ `ErrOldServer` (م9)؛ احراز ناموفق در لاگ سرور رد ندارد (م10)؛ پشتیبان شکسته کاوشگر را بی‌پاسخ می‌گذارد (م16)؛ منابع کاوشگرهای ارسال‌شده بی‌سقف (م13).
50. تعداد لینک‌های موازی و طول عمرشان بزرگ‌ترین نشانهٔ رفتاری‌اند (AUC ~۱٫۰ و ~۰٫۹۳)؛ عمداً حفظ شده (تصمیم صاحب پروژه).
51. کلید مشترک در لینک `hs2://` به‌صورت base64 آشکار است؛ پشتیبان‌ها کلید و کلید خصوصی گواهی را دارند؛ کلاینت گواهی را وارسی نمی‌کند (`InsecureSkipVerify`) ولی پیام‌های نصب‌کننده خلاف آن را القا می‌کنند.

### ۱۲.۶ عملیات، پیکربندی و نصب‌کننده

52. `run` `mode` را اعتبارسنجی نمی‌کند و `unhex` خطای هگز را نادیده می‌گیرد؛ `check` تهی بودن `local_cidr`/`peer_ip`، ناهمخوانی MTU دو سمت، و نام cc/qdisc را نمی‌سنجد؛ کلیدهای زیر `tuning` بررسی نمی‌شوند.
53. سیگنال‌های `SetTCPMemPressure`/`SetHostSaturated` به نویسندهٔ وضعیت وابسته‌اند و بی `/run/hs2` بی‌صدا خاموش‌اند.
54. sysctlها فقط در شروع اعمال می‌شوند، با چند تونل آخرین شروع‌شده برنده است، و هنگام توقف برگردانده نمی‌شوند؛ `hs2 tune` ممکن است `modprobe` کند؛ آستانه‌ها روی `MemTotal` است (سرور «۴ گیگ» با MemTotal ≈۳۸۰۰ و ۲ هسته ⇒ medium).
55. دو راه ساخت l3mtcp با MTU متفاوت (۱۳۲۰ / ۱۳۸۰)؛ `check` ۵۷۶..۹۰۰۰ و نصب‌کننده ۶۸..۶۵۵۳۵.
56. `hs2 doctor` برای mtcp احتمالاً هشدار کاذب `tun hs0: not present` می‌دهد؛ آستانه‌های تازگی ناهمسان (>۶s در status/doctor، ≤۷s در ports/نصب‌کننده)؛ PSI ۱۰ ثانیه‌ای در زنده و ۶۰ ثانیه‌ای در doctor.
57. خطای bind پورت کاربر کل دیمن را می‌کشد؛ خروجی معکوس همیشه از `WarmSize` شروع می‌کند؛ `per_link` روی خروجی معکوس بی‌اثر است؛ نگهبان خاموشی همیشه کد ۰.
58. نصب‌کننده: آزمون e2e (`drv.py`) کهنه و احتمالاً شکست‌خورده؛ `tm_edit_test.sh` ناپایدار؛ پشتیبان drop-inها و `/etc/modules-load.d/hs2.conf` را ندارد؛ بازگردانی پشتیبان قدیمی ممکن است `99-hs2.conf` را برگرداند؛ `jget`/`jraw` تجزیهٔ JSON با grep؛ وابستگی به apt؛ پیش‌فرض منوی Transport «auto» که پورت کاربر ندارد.

### ۱۲.۷ ناسازگاری مستندات با کد

| سند | ادعا | واقعیت |
|---|---|---|
| `README.md:18-19` (و `hs2-src/README.md:18-19`) | لینک مرده حدود ۲s تشخیص داده می‌شود | فقط برای بستن صریح (FIN/RST)؛ سیاه‌چاله: suspect ۱۲s و مرگ با `USER_TIMEOUT` ۲۰s یا keepalive ۲۴s |
| `README.md:389` | بازکاوش پلیس‌گر ۵٪ هر ۱۰s | ۱۵٪ هر ۴s (`udpcarrier/governor.go:125-126`) |
| `README.md:588-590` و `CHANGELOG.md:1477-1483` | حامل بیکار در camo ساکت می‌شود | CA1b ضربان ≈۰٫۷s |
| `README.md:627` | نصب‌کننده تا ۴۰s صبر می‌کند | ۶۰s (`HS2_VERIFY_SECS`) |
| `README.md:756-758` | `auto` به tcp/TLS (mtcp، l3mtcp، tls) برمی‌گردد | به `noise` روی TCP (`engine/carrier_udp.go:86-90`) |
| `README.md:839-840`، `hs2-src/README.md:151-152` | نصب‌کننده `99-hs2.conf` می‌نویسد | نصب‌کننده آن را **پاک** می‌کند؛ qdisc پیش‌فرض `fq_codel` |
| `README.md:471` | تغییر اندازهٔ پول جریان زنده را جابه‌جا نمی‌کند (dgtun) | پس از ۳۰s retire و هنگام mute جابه‌جا می‌شود |
| `README.md:10` | هرگز TCP-in-TCP | فقط برای پورت‌های کاربر |
| `README.md:680-691` | جدول v2→v3 | احتمالاً پیش از autopilot سنجیده شده (**نامطمئن**) |
| `hs2-src/README.md` (کل)، `BUILD.md` | «8–16 links»، چیدمان و فرمان‌ها و آزمایشگاه قدیمی | کهنه |
| `engine/stream.go:40` | `kindInfo` «display only» | پنج اثر رفتاری (دروازهٔ info، `capL3Quiet`، برچسب پورت، growable/پیش‌فرض، سقف همتا)؛ فقط دربارهٔ **سقف** درست است |
| `cmd/hs2/main.go:692`، `engine/stream_iran.go:28-29`، `udpcarrier/carrier.go:163, 220-221` | توضیح‌های کهنه | ناهمخوان با کد |
| `run.sh:90` | ستون `link_churn` | الگو با لاگ امروز نمی‌خواند ⇒ همیشه صفر |

---

## ۱۳. واژه‌نامه و جدول ثابت‌ها

### ۱۳.۱ واژه‌نامه

| واژه | معنی در این سند |
|---|---|
| لبه (edge) | سرور ایران (`mode: dial`)؛ کاربران به آن وصل می‌شوند؛ کلاینت smux؛ همهٔ تصمیم‌های پول |
| خروجی (exit) | سرور خارج (`mode: listen`)؛ پنل پشت آن؛ سرور smux |
| مستقیم / معکوس | چه کسی TLS را dial می‌کند؛ مستقیم = لبه، معکوس = خروجی |
| لینک (link) | یک اتصال TCP+TLS+smux بین دو سرور |
| پول (pool) | مجموعهٔ لینک‌ها؛ `LinkManager` روی لبه و `exitPool` روی خروجی معکوس |
| hs0 | رابط TUN کانال جانبی در l3mtcp |
| کانال L3 | جریان خام `kindL3` روی هر لینک که بسته‌های hs0 را می‌برد |
| جریان خام (raw stream) | جریانی که با `OpenRawStream` باز می‌شود و «کاربر» شمرده نمی‌شود (کنترل، آمار، info، pool، L3) |
| T (target) | تعداد لینک‌های serving که autopilot می‌خواهد |
| serving | لینکی که اتصال تازه می‌پذیرد: `!retiring && !degraded && !draining && alive && !suspect` |
| retiring | بازنشسته: اتصال تازه نمی‌گیرد، با خالی شدن بسته می‌شود؛ برگشت‌پذیر |
| born-spare | لینکی که در معکوس وقتی `S ≥ T` است می‌رسد و از آغاز retiring است |
| suspect | ≥۱۲s بی‌دریافت؛ بیرون از لایه‌های ۰ و ۱؛ برگشت‌پذیر |
| degraded / draining | حکم loss یا stuck؛ تخلیهٔ پله‌ای؛ برگشت‌ناپذیر |
| stuck | زیرنوع degraded: پینگ کنترل ≥۶s منتظر با شاهد سالم |
| pressed | فرستندهٔ لینک ≥۵۰٪ زمان منتظر شبکه با ≥16KiB/تیک و rwnd<۵۰٪، ۲ از ۳ |
| flowing | جریان کاربری با EWMA ≥2KiB/s و فعالیت ۶s اخیر، یا ≥256B/s در ۳ نمونه |
| `fl5` / `fl60` | کمینهٔ flowing در ۱۰s / بیشینه در ۶۰s |
| `spare(p)` | لینک بی‌فشاری که باید برای p لینک pressed بماند |
| پروب (probe) | افزایش آزمایشی T و داوری سود آن |
| H | اندازه‌ای که تقاضای اخیر لازم دارد؛ هدف کوچک‌سازی |
| U | بیشینهٔ لینکی که جریان‌های فعال می‌توانند به کار ببرند |
| `drainHeadroom(n)` | `max(2, ceil(n/8))`؛ سقف تخلیه/جایگزین هم‌زمان |
| wedge | خوانندهٔ smux پارک‌شده چون سطل نشست پر است |
| squeeze | آزادسازی رله‌های گیر زیر فشار حافظهٔ TCP |
| refill hold | نگه‌داشتن اتصال‌های تازه تا لینک‌ها بالا بیایند، پس از شروع/قطعی کامل |
| dial gate | محدودکنندهٔ سراسری handshakeها |
| شاهد (answering) | لینک پرکار سالمی که پینگ بعدتری را زیر ۲s پاسخ گرفته |
| مسیر کند / پنجرهٔ بهبود | حالتی که هیچ حکم loss/stuck صادر نمی‌شود |
| EKM | Exported Keying Material (TLS exporter) برای احراز متصل به نشست |
| rendezvous hashing | انتخاب لینک با بیشینهٔ `mix32(hash ^ id)` |
| flowlet | دنبالهٔ بسته‌های یک جریان بی‌مکث ≥۳۰۰ms (dgtun) |
| **[dg]** | فقط dgtun / udpcarrier |

### ۱۳.۲ جدول ثابت‌ها (نام | مقدار | path:line)

مسیرها نسبت به `hs2-src/`؛ `smux@` = `smux@v1.5.24`.

**سوکت و TLS**

| نام | مقدار | path:line |
|---|---|---|
| `NotSentLowat` | 32KiB | `tlscarrier/tune_linux.go:19` |
| `UserTimeoutMs` | 20000 | `tlscarrier/tune_linux.go:22` |
| `CongestionControl` | `bbr` | `tlscarrier/tune_linux.go:29` |
| keepalive TCP شماره‌گیر | 3s (فقط `TCP_KEEPIDLE`) | `tlscarrier/carrier.go:183-190` |
| connect `DialFrom` / پیشاهنگ معکوس | 8s / 2s | `tlscarrier/carrier.go:162`؛ `cmd/hs2/main.go:535` |
| `authTimeout` / `handshakeTimeout` / `firstReadTimeout` | 10s / 10s / 30s | `tlscarrier/auth.go:59`؛ `tlscarrier/server.go:40, 47` |
| `writeTimeout` (حامل خام؛ نه مسیر smux) | 5s | `tlscarrier/carrier.go:27` |
| `authClockSkew` | ±2 سطل دقیقه‌ای | `tlscarrier/auth.go:58, 91` |
| `replayTTL` / `replayCap` | 5m / 16384 | `tlscarrier/replaymem.go:16-17` |
| توزیع `LengthSampler` | ۹ اندازه ۴۰..۱۴۰۰، میانگین ≈۹۹۱ | `obfs/shaper.go:41-42` |
| `shapeHdrLen` / `shapeMaxFrame` | 4 / 32KiB | `engine/shape.go:47, 50` |

**smux و stream**

| نام | مقدار | path:line |
|---|---|---|
| `SmuxFrameSize` / `SmuxStreamBuffer` / `SmuxSessionBuffer` | 16KiB / 2MiB / 8MiB | `engine/mtcp_link.go:258, 262, 264` |
| `Version` | 2 | `engine/mtcp_link.go:269` |
| `KeepAliveInterval` / `KeepAliveTimeout` | 4000+rand(4000)ms / 24s | `engine/mtcp_link.go:278-279` |
| `openCloseTimeout` | 30s | `smux@/session.go:17` |
| پنجرهٔ اولیهٔ همتا / آستانهٔ UPD | 256KiB / 1MiB | `smux@/frame.go:28`؛ `smux@/stream.go:146` |
| `copyBufs` | 32KiB | `engine/stream.go:53` |
| `kindTimeout` | 10s | `engine/stream.go:51` |
| dial پنل در خروجی | 5s | `engine/stream_kharej.go:233` |
| `pickWait` | 40 × 150ms | `engine/stream_iran.go:184, 206` |
| تلاش `openStream` | 3 | `engine/stream_iran.go:243` |
| `udpFlowQueue` / `udpFlowBytes` / `udpIdle` | 256 / 512KiB / 2m | `engine/stream_iran.go:286-289`؛ `engine/stream.go:270` |
| `relayDieGrace` | 5s | `engine/wedge.go:68-70` |

**کانال L3**

| نام | مقدار | path:line |
|---|---|---|
| `l3QueueLen` | 256 | `engine/l3_link.go:53` |
| `l3BatchBytes` | 16KiB | `engine/l3_link.go:56` |
| `l3MaxSojourn` | 60ms | `engine/l3_link.go:62` |
| `l3KeepaliveEvery` / `l3DeadAfter` (بی‌استفاده در تولید) | 2s / 8s | `engine/l3_link.go:67-68` |
| `l3StreamDeadAfter` / `l3QuietKeepalive` | 30s / 10s (±۲۰٪) | `engine/l3_link.go:78-79` |
| `l3SessionSilent` (تیک ۲s) | 12s | `engine/l3_link.go:87, 144` |
| مهلت `WriteRaw` | 5s | `engine/stream.go:214-218` |
| سقف قاب L3 | 65536 | `engine/stream.go:227` |
| MTU پیش‌فرض باینری / txqueuelen | 1380 / 2000 | `cmd/hs2/main.go:458-461`؛ `tun/tun_linux.go:125` |
| `capL3Quiet` | `1<<1` | `engine/peerinfo.go:55` |

**پول و سلامت**

| نام | مقدار | path:line |
|---|---|---|
| `healthTick` | 2s | `engine/health.go:18` |
| `activeBytes` | 96KiB/تیک | `engine/health.go:25` |
| `lossFrac` / `degradeStreak` | 0.12 / 3 | `engine/health.go:31, 34` |
| `flowTau` / `flowingRate` / `flowRecent` / `flowSteadyRate` | 10s / 2KiB/s / 6s / 256B/s | `engine/health.go:41-48` |
| `blockedMin` | 1ms | `engine/health.go:53` |
| `maxDrain` / `drainStall` / `maxDrainActive` | 45s / 15s / 90s | `engine/health.go:74-76` |
| `warmStartLinks` | 8 | `engine/health.go:85` |
| `retireAfterDrop` | 4s | `engine/health.go:90` |
| `controlInterval` | 3s | `engine/health.go:95` |
| `pressBlocked` / `pressMinBytes` / `rwndShareMax` | 0.5 / 16KiB / 0.5 | `engine/linkmanager.go:70-72` |
| `statsStale` / `statsGap` | 6s / 7s | `engine/linkmanager.go:75-76` |
| `drainIdleDefault` / `retireForce` | 310s / 20m | `engine/linkmanager.go:82, 91` |
| `suspectAfter` | 12s | `engine/linkmanager.go:317` |
| `pickWindow` | 3 | `engine/linkmanager.go:1497` |
| `drainHeadroom(n)` | `max(2, ceil(n/8))` | `engine/linkmanager.go:1253` |
| `closesPerTick(r)` | `min(max(2, ceil(r/32)), 8)` | `engine/linkmanager.go:1062-1064` |
| `jitterGap` / `closeJitter` | 40+U[0,120)ms / 50+U[0,200)ms | `engine/linkmanager.go:394-401` |
| `dialFailRun` | 3 | `engine/linkmanager.go:865` |
| `gateInflight` / `gatePerSec` | 8 / 10 | `engine/dialgate.go:22, 26` |
| `ctrlPending` / `ctrlBusyBytes` | 4 / 4KiB | `engine/control.go:43, 47` |
| `stuckWait` / `stuckStreak` / `stuckPrompt` | 6s / 2 / 2s | `engine/stuck.go:62-69` |
| `stuckRecover` / `stuckRecoverMax` | 30s / 2m | `engine/stuck.go:75-76` |
| `stuckMoveFloor` | 12KiB/تیک | `engine/stuck.go:82` |
| `stuckInflate` / `stuckInflateFloor` / `stuckBaseMins` | 4 / 500ms / 10 | `engine/stuck.go:91-93` |
| `lossPathMin` / `lossWinMin` / `lossQuietKeep` / `lossKeepShare` | 4 / 1s / 20s / 0.5 | `engine/loss.go:56-67` |
| `wedgeLooks` / `starveCalls` / `stuckFor` / `guardTick` | 3 / 2048 / 6s / 2s | `engine/wedge.go:48-62` |
| `refillHoldMax` / `refillStall` / `refillTick` / `refillRearm` | 10s / 3s / 100ms / 1m | `engine/refill.go:50-57` |
| `statsReopenAfter` / `statsHandshakeTimeout` | 5s / 5s | `engine/stats.go:43, 100` |
| `infoTimeout` / `infoRetry` / `infoTries` / `infoSlowRetry` | 5s / 10s / 3 / 1m | `engine/peerinfo.go:52, 85-89` |
| فشار حافظهٔ TCP | روشن `≥ tcp_mem[1]`، خاموش `< 0.9·tcp_mem[1]` | `cmd/hs2/status.go:845-853` |

**autopilot** (همه در `engine/autopilot.go`)

| نام | مقدار | خط |
|---|---|---|
| `histTicks` / `shortWin` / `shortN` | 30 / 5 / 3 | `:129-131` |
| `armTimeout` + `armPerLink` | 15s + 150ms/لینک | `:132, 306` |
| `settleTicks` / `looks` / `baseTicks` | 2 / {5,10,15} / 10 | `:133-135` |
| `z` / `additivity` / `minGain` / `earlyFail` | 2.5 / 0.5 / 0.05 / 0.25 | `:136-139` |
| `rMinAbs` / `rMinFrac` | 32KiB/s / 0.25 | `:140-141` |
| `backoffBase` / `backoffMax` / `jitter` | 30s / 8m / 0.2 | `:142-144` |
| `successNext` / `inconclusiveNext` / `abortNext` | 4s / 30s / 60s | `:145-147` |
| `capWindow` / `capMinSamples` / `capMax` | 30m / 6 / `max(256, 4·max)` | `:148-150, 272-274` |
| `util` / `minBWForNeed` | 0.7 / 16KiB/s | `:151-152` |
| `shrinkDwell` / `shrinkStep` / `noShrinkAfterGrow` / `overshootWin` | 60s / 30s / 60s / 60s | `:153-156` |
| `holdBase` / `holdMax` / `holdVoid` / `undoWindow` | 10m / 2h / 0.6 / 2h | `:157-160` |
| `kResetAfter` / `chainWindow` / `activeRate` | 30m / 60s / 8KiB/s | `:161-163` |
| `confirmWin` / `ceilTTL` | 60s / 1h | `:164-165` |
| `probeStepMax` / `probeChainStepMax` | 32 / 64 | `:302-303` |
| `spare(p)` | `min(ceil(p/4), max(4, ceil(p/16)))` | `:284-296` |

**معکوس و خروجی**

| نام | مقدار | path:line |
|---|---|---|
| `poolCtlInterval` / `poolCtlSlow` / `poolCtlSpread` | 3s / 30s / 1.5s (با `jitterAround` ±۲۰٪) | `engine/exit_pool.go:32, 450, 466, 528-537` |
| `slotBackoffMin` / `slotBackoffMax` / `scoutBackoffMax` / `slotStableAfter` | 500ms / 8s / 2s / 30s | `engine/exit_pool.go:48-55` |
| `reverseAcceptSlack` / `reverseRefuseHold` | 8 (سقف `2×max+8`) / 5s | `engine/stream_reverse.go:36-37` |
| `bornSpareGrace` | 30s | `engine/linkmanager.go:308` |

**پیکربندی، سقف و عملیات**

| نام | مقدار | path:line |
|---|---|---|
| `min_links` / `per_link` پیش‌فرض | 2 / 8 | `cmd/hs2/main.go:646-655` |
| `legacyMaxLinks` / `icmpMaxLinks` | 32 / 8 | `cmd/hs2/main.go:678, 686` |
| `maxLinksLow/Medium/High` | 32 / 48 / 64 | `tune/tune.go:199-201` |
| `LinkWorstCaseMiB` / `LinkRAMPerLinkMB` / `MaxLinksCap` / `maxLinksFewCores` | 12 / 48 / 300 / 128 | `tune/tune.go:206-212` |
| `statusInterval` / `warmMaxAge` / `warmAfter` | 2s / 15m / 1m | `cmd/hs2/status.go:29, 201, 236` |
| `hostSatBusy` / `hostSatPSI` / `hostSatRuns` | 90 / 40 / 3 | `cmd/hs2/hostcpu.go:59-61` |
| `hostClearBusy` / `hostClearPSI` / `hostClearRuns` | 75 / 20 / 5 | `cmd/hs2/hostcpu.go:62-64` |
| `tcp_notsent_lowat` سیستمی | 131072 | `tune/tune.go:352` |
| پیش‌فرض MTU نصب‌کننده | راه tun: ۱۳۲۰ (۶۸..۶۵۵۳۵)؛ راه tcp: ۱۳۸۰ ثابت | `install.sh:1660-1672, 1875` |
| `RestartSec` / `TimeoutStopSec` / `LimitNOFILE` | 3 / 8 / 1048576 | `install.sh:824-849` |
| `HS2_VERIFY_SECS` | 60 | `install.sh:902` |

**[dg] برای مقایسه**

| نام | مقدار | path:line |
|---|---|---|
| `dgQueueLen` / `dgSojourn` / `dgWarmGrace` | 256 / 50ms / 4s | `engine/dgpool.go:49-54` |
| `flowletGap` / `dgRetireForce` | 300ms / 30s | `engine/dgpool.go:347, 1608` |
| `dgMuteAfter` / `dgSilentDead` / `dgPeerMuteFor` | 1s / 3s / 4s | `engine/dgpool.go:74, 61, 85` |
| `dgReorderHold` | 15ms | `engine/reorder.go:49` |
| `deadAfter` حامل UDP / `feedbackEvery` | 15s / 100ms | `udpcarrier/carrier.go:54-55` |
| FEC حامل `K` / `Window` / `MaxDepth` | 8 / 60ms / 64 | `udpcarrier/carrier.go:167` |
| `targetQueue` / `lowQueue` / `baseProbeEvery` / `startupGain` | 10ms / 5ms / 4s / 2.885 | `udpcarrier/rate.go:231-237, 257` |
| `pacerQueueTime` / `pacerQuantum` / `pacerLateCredit` / `pacerSatCredit` | 20ms / 2ms / 10ms / 50ms | `udpcarrier/pacer.go:82-120` |
| `govTickEvery` / `govCapFrac` / `govRaiseEvery`·`govRaiseGain` / `govRest`→`govRestMax` | 500ms / 0.9 / 4s·1.15 / 5m→1h | `udpcarrier/governor.go:108-133` |
| `maxRecoverableLoss` (`auto`) | 0.45 | `engine/carrier_udp.go:24` |

---

## پیوست: فهرست نقشه‌ها و موضوع هرکدام

همهٔ نقشه‌ها در پوشهٔ `maps/` کنار همین سند هستند. آزمایش‌هایی که در نقشه‌ها به پوشهٔ `scratchpad` ارجاع می‌دهند در محیط موقت بررسی اجرا شده‌اند و جزو مخزن نیستند.

| فایل | موضوع | بخش‌های این سند که از آن ساخته شده |
|---|---|---|
| `01-cmd-runtime.md` | راه‌اندازی و سیم‌کشی `cmd/hs2`: `runCmd`، `runStream`، پاکت پول، warm، وضعیت، گواهی، check/config/ports، کلیدهای پیکربندی | ۱، ۲.۱، ۹ |
| `02-cmd-ops-tune.md` | بستهٔ `tune`، فایل وضعیت، `hs2 status/doctor/tune/recommend-links`، سقف لینک، پایش CPU و حافظه | ۹.۲–۹.۶ |
| `03-stream-core.md` | هستهٔ stream: پشتهٔ لینک، smux، شکل‌دهی، `watchConn`، جریان‌های کاربر و کنترلی، معکوس | ۲، ۳ |
| `04-linkmanager.md` | `LinkManager`: تیک، Pick، reconcile، dial، heal، drain، معکوس، ماشین حالت، خط‌های زمانی خرابی | ۴، ۶ |
| `05-autopilot-health.md` | autopilot با همهٔ عددها، سیگنال‌های فشار، loss، stuck، wedge، فشار حافظه، refill، dial gate | ۵، ۶ |
| `06-l3-tun.md` | کانال جانبی hs0: باز شدن TUN، صف، rendezvous، keepalive، ناظر نشست، MTU | ۲، ۱۱.۲، ۱۲.۱ |
| `07-tls-obfs-core.md` | tlscarrier، احراز EKM، پوشش، شکل‌دهی، core/noise/reality | ۳، ۱۲.۵ |
| `08-dgtun.md` | dgtun: پول datagram، sticky، fq، reorder، mute، scout، مقایسه با l3mtcp | ۸ |
| `09-udp-fec.md` | حامل UDP، کنترل نرخ، pacer، Governor، FEC، `auto` | ۸.۳ |
| `10-encap.md` | encap: سوکت خام، ابهام ICMP، echoguard، استتار CA (بخش‌های ۱۲–۱۴ در ۹۰ §۱۱ تکمیل شد) | ۸.۴ |
| `11-install.md` | `install.sh`: منوها، دو راه l3mtcp، قالب‌ها، لینک `hs2://`، unit، ارتقا و پشتیبان | ۹.۷ |
| `12-history.md` | تاریخچهٔ فازها از پیام ثبت‌ها و CHANGELOG، عددهای سنجیده، ایده‌های ردشده، پسرفت‌ها، مسائل باز | ۱۰ |
| `13-docs-lab.md` | README/BUILD/VALIDATION، ابزارهای آزمایشگاه، نتایج ثبت‌شده، ناسازگاری مستندات | ۹.۸، ۱۲.۷ |
| `20-l3mtcp-e2e.md` | ردیابی سرتاسری l3mtcp: مسیر بسته، اتصال TCP، صف‌ها، خط‌های زمانی خرابی، آزمون‌های آزمایشی | ۲، ۶.۳ |
| `21-control-loops.md` | کاتالوگ L01–L41 و D01–D21، ماتریس تعامل، جدول F1–F31، شکاف‌ها | ۷ |
| `90-completeness.md` | پوشش فایل‌ها و آزمون‌ها، مسیریابی هر پورت، موتور تک‌حاملی، `hs2 config`، تبار باینری، «آنچه وجود ندارد» | ۱۱.۱۲، ۱۲، ۳.۶ |
| `91-verify.md` | راستی‌آزمایی تقابلی ۸۱ ادعا (۶۹ تأیید، ۱۱ بخشی، ۰ رد، ۱ نامطمئن) و اصلاحیه‌ها | سراسر سند (اصلاحیه‌ها بر نقشه‌ها مقدم‌اند) |
