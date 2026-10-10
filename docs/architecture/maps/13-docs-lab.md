# مستندات، اعتبارسنجی و آزمایشگاه

> نقشهٔ زیرسیستم «مستندات + اعتبارسنجی میدانی + آزمایشگاه» در مخزن hs2 (شاخهٔ برابر main، ثبت `0812bc9`، باینری منتشرشده `build da621f5db2bb 2026-10-07` — با اجرای `./hs2-linux-amd64 version` بررسی شد).
> همهٔ شماره‌خط‌ها نسبت به همین درخت است. «تأیید با کد» یعنی مقدار را در کد دیدم؛ «ناسازگار» یعنی مستند با کد نمی‌خواند؛ «نامطمئن» یعنی نتوانستم قطعی کنم.
> هیچ فایلی در مخزن تغییر داده نشد.

**فایل‌هایی که کامل خوانده شد:**
`README.md` (841 خط)، `hs2-src/README.md` (153)، `hs2-src/BUILD.md` (204)، `hs2-src/VALIDATION.md` (287)، همهٔ `hs2-src/lab/*`: `run.sh` (93)، `sweep.py` (71)، `analyze.py` (37)، `tcpload.py` (46)، `dgtun.sh` (118)، `encap.sh` (68)، `cpuquota.sh` (208)، `transport-probe.sh` (235)، `netem/main.go` (439)، `netsim/netsim.go` (371)، `probe/main.go` (342)، `dglab/main.go` (394).
برای سنجش ادعاها این‌ها هم خوانده یا جست‌وجو شد: `cmd/hs2/main.go` (بخش بزرگ)، `engine/l3_link.go` (کامل)، `engine/health.go`، `engine/autopilot.go` (ثابت‌ها)، `engine/linkmanager.go` (ثابت‌ها)، `engine/refill.go`، `engine/stuck.go`، `engine/loss.go`، `engine/wedge.go`، `engine/dialgate.go`، `engine/mtcp_link.go`، `engine/dgpool.go` (ثابت‌ها و خط carriers)، `engine/control.go`، `engine/stream.go`، `engine/carrier_udp.go`، `tlscarrier/tune_linux.go`، `tune/tune.go`، `udpcarrier/carrier.go`، `udpcarrier/governor.go`، `udpcarrier/pacer.go`، `encap/obfs.go`، `encap/echoguard_linux.go`، `cmd/hs2/status.go`، `cmd/hs2/check.go`، `udpcarrier/lab_test.go`، `engine/carrier_udp_test.go`، `install/tests/release_files_test.sh`، و بخش‌های مرتبط `CHANGELOG.md`، `udpcarrier/REPORT.md`، `fec/README.md`، `install/e2e/README.md`.

---

## 1. نقش و جایگاه در کل سیستم

این زیرسیستم کد اجرایی تونل نیست. سه نقش دارد:

1. **قرارداد با کاربر و اپراتور (مستندات):**
   - `README.md` ریشه مرجع اصلی و به‌روز کاربر است: معماری v3، الگوی تطبیقی پیوندها (autopilot)، سقف پیوندها، رفتار در راه‌اندازی دوباره و قطعی، تونل datagram، استتار ICMP، امنیت، نصب و مدیریت.
   - `hs2-src/README.md` **نسخهٔ قدیمی‌ترِ همان README** است (پیش از v3.2 و autopilot). چند جای آن با کد امروز نمی‌خواند (بخش 13).
   - `hs2-src/BUILD.md` ساخت، چیدمان کد، شرح autopilot، کربرها، امنیت و آزمایشگاه را می‌گوید.
   - `hs2-src/VALIDATION.md` چک‌لیست اعتبارسنجی **روی سرور واقعی** است (سناریوهای V1 تا V14، معیار برگشت، خطوط لاگی که باید پایید).
2. **آزمایشگاه قابل تکرار (`hs2-src/lab/`):** شبیه‌ساز مسیر در فضای کاربر (`netem`)، شبیه‌ساز درون‌فرایندی (`netsim`)، ابزار سنجش (`probe`، `dglab`، `tcpload.py`)، اسکریپت‌های یک‌آزمایشی (`run.sh` برای حامل‌های جریانی، `dgtun.sh` برای تونل datagram، `encap.sh` برای یک حامل datagram خام، `cpuquota.sh` برای فرستندهٔ کم‌پردازنده) و ابزار جاروب و تحلیل (`sweep.py`، `analyze.py`). یک ابزار هم برای **مسیر واقعی** است: `transport-probe.sh`.
3. **سابقهٔ تصمیم‌ها:** نتایج آزمایشگاهی و میدانی و ایده‌های ردشده عمدتاً در `CHANGELOG.md` (1809 خط) و چند گزارش جانبی (`udpcarrier/REPORT.md`، `fec/README.md`) ثبت شده‌اند. README و VALIDATION مدام به آن ارجاع می‌دهند.

**ارتباط با تونل اصلی کاربر (l3mtcp):**
- l3mtcp همان هستهٔ جریانی mtcp است به‌اضافهٔ کانال جانبی `hs0`: `cmd/hs2/main.go:409-410` → `runStream(ctx, fc, true, 0)`.
- مستندات دربارهٔ آن این‌ها را می‌گویند:
  - `README.md:659-666`: کانال جانبی فقط برای ping و ترافیک سبک غیر‌TCP است، با سقف 60 ms زمان صف.
  - `README.md:234-235`: جابه‌جایی جریان‌های TUN پس از 12 s سکوت نشست.
  - `README.md:685`: نتیجهٔ آزمایشگاهی `throttled, l3mtcp`.
  - `BUILD.md:145-153`: جدول کربرها.
- در آزمایشگاه، l3mtcp را `run.sh` با `MODE=l3mtcp` می‌آزماید؛ در این حالت ping روی `hs0` هم گرفته می‌شود (`run.sh:80-84`).
- در `transport-probe.sh:56` نام l3mtcp در جدول نتیجه `tun-tcp` است.

---

## 2. اجزای اصلی

### 2.1 مستندات

| سند | بخش‌ها (خط) | محتوای کلیدی |
|---|---|---|
| `README.md` | 1-19 معرفی؛ 21-107 autopilot؛ 109-181 سقف پیوند؛ 183-247 راه‌اندازی دوباره، قطعی و خوانندهٔ گیرکرده؛ 249-261 تنظیم هسته؛ 263-285 گواهی؛ 287-326 چند تونل؛ 328-376 مقصد جدا برای هر درگاه؛ 378-555 تونل datagram (پلیس‌گر، وضعیت، CPU، صف منصفانه، سهم منصفانه، offload، حامل قطع‌شده)؛ 557-622 ICMP روی سیم و استتار؛ 624-655 بررسی‌های نصب‌کننده؛ 657-691 تغییرات v3 و جدول آزمایشگاه؛ 693-752 نصب و ارتقا؛ 754-774 حالت‌های جریانی؛ 776-800 امنیت؛ 802-841 مدیریت و درگاه‌ها | مرجع کاربر، تقریباً هم‌گام با کد (استثناها در بخش 13) |
| `hs2-src/README.md` | 21-50 تغییرات v3 و همان جدول؛ 106-118 حالت‌ها با «8–16 links»؛ 120-128 امنیت؛ 130-136 مدیریت | نسخهٔ پیش از autopilot؛ کهنه |
| `hs2-src/BUILD.md` | 1-44 ساخت، `CGO_ENABLED=0`، هش، مهر ساخت، آزمون با `-race`؛ 46-57 چیدمان؛ 59-69 فرمان‌ها؛ 71-126 autopilot و LinkManager و حالت reverse؛ 128-136 `tune/`؛ 138-156 کربرها؛ 158-163 سازگاری سیمی؛ 165-180 مدل امنیت؛ 182-204 آزمایشگاه | تنها جایی که سازوکار آزمایشگاه (run.sh و sweep و analyze) و متغیرهای `HS2_TUNE_*` مستند شده‌اند |
| `hs2-src/VALIDATION.md` | 15-31 برگشت؛ 33-59 خطوط لاگ؛ 61-272 سناریوهای V1 تا V14؛ 274-287 گزارش و شناسهٔ انتشار | اعتبارسنجی میدانی Q7، Q8، V، W، X، Y و CA |
| `CHANGELOG.md` (خارج از دامنهٔ اصلی؛ فقط بخش‌های مرتبط خوانده شد) | 719-1097 فاز Q (Q1 تا Q8، آزمون بار)؛ 1099-1232 فاز V؛ 1234-1466 فاز W؛ 1468-1512 CA؛ 1514-1566 Y؛ 1568-1795 X؛ 1797-1809 روش راستی‌آزمایی | منبع اصلی نتایج و ایده‌های ردشده |
| `udpcarrier/REPORT.md` | 67-113 کارایی (netsim و netem)؛ 115-145 محدودیت‌ها؛ 237-246 نکتهٔ آزمایشگاه | مقایسهٔ UDP و TCP زیر 26٪ اتلاف انفجاری |
| `fec/README.md` | 31-64 پیش‌فرض‌ها و اندازه‌گیری‌ها | چرایی K=32، Window=30 ms و … |
| `install/e2e/README.md` | کل | آزمون سرتاسری نصب‌کننده (Docker، systemd، pexpect، حدود 70 رفتار) |

### 2.2 ابزارهای آزمایشگاه (`hs2-src/lab/`)

#### `netem/main.go` — شبیه‌ساز مسیر لایهٔ 2 در فضای کاربر (فقط لینوکس، root)
- **چرا هست:** هستهٔ آزمایشگاه `sch_netem` ندارد (`netem/main.go:13`؛ `BUILD.md:184-186`). دو رابط را با سوکت `AF_PACKET` به هم پل می‌زند. قاب‌ها دست‌نخورده عبور می‌کنند، پس TCP مثل مسیر واقعی رفتار می‌کند.
- **`dirCfg` (`:38-71`)**، تنظیمات هر جهت:
  - `rate`، `delay`، `jitter` (که باعث جابه‌جایی ترتیب بسته‌ها می‌شود)، `queue` (حداکثر تأخیر صف پیش از دورریختن از ته صف)، `loss`؛
  - `flowRate`/`flowBurst`: پلیس‌گر جداگانه برای هر جریان، به شیوهٔ DPI؛
  - پلیس‌گر جداگانه برای هر IP مقصد، دوره‌ای: `dstRate`، `dstBurst`، `dstPenalty`، `dstPenLoss`، `dstProto`؛
  - اتلاف انفجاری Gilbert-Elliott زمان‌محور: `burstLoss`، `burstGoodMs`، `burstBadMs`؛
  - `dropUDP` و `allow`: فهرست پروتکل‌های IP مجاز؛ مثلاً `1` یعنی مسیری که فقط ICMP رد می‌کند.
- **`drop` (`:88-108`):** اتلاف i.i.d. یا GE. در GE مدت هر حالت توزیع نمایی دارد.
- **`flowKey` (`:156-178`):** کلید جریان از دید DPI:
  - TCP و UDP: پنج‌تایی؛
  - ICMP echo: (مبدأ، مقصد، شناسهٔ echo)؛
  - GRE کلیددار: (مبدأ، مقصد، کلید)؛
  - بقیهٔ پروتکل‌ها: (مبدأ، مقصد، پروتکل)؛ پس چند پیوند روی پروتکل بی‌درگاه برای چنین جعبه‌ای یک جریان‌اند.
- **`run` (`:207-352`):** یک goroutine برای هر جهت. ترتیب پردازش هر قاب:
  1. نادیده گرفتن قاب‌های خروجی خودی (`PACKET_OUTGOING`، `:262`)؛
  2. `dropUDP`؛
  3. `allow`؛
  4. اتلاف؛
  5. پلیس‌گر مقصد (`:281-310`)؛
  6. پلیس‌گر جریان (`:311-329`)؛
  7. ساعت گلوگاه و دورریختن از ته صف (`:330-340`)؛
  8. تأخیر و jitter (`:343-349`)؛
  9. نوشتن با heap بر پایهٔ زمان تحویل (`:213-248`).
- **`counters` (`:354-363`):** آمار با کلید `dir.kind`. هنگام SIGTERM یا SIGINT به stderr چاپ می‌شود (`:433-438`)؛ `encap.sh` آن را برمی‌دارد.
- **پرچم‌ها (`:384-404`):** `-rate`، `-rate-ba`، `-delay`، `-jitter`، `-jitter-ba`، `-queue` (پیش‌فرض 200ms)، `-loss`، `-burstloss`، `-goodms` (150)، `-badms`، `-flowrate`، `-flowburst` (64 KiB)، `-dropudp`، `-allow`، `-dstpolice`، `-dstburst` (5s)، `-dstpenalty` (1s)، `-dstpenloss` (0.8)، `-dstproto`.

#### `netsim/netsim.go` — شبیه‌ساز UDP درون‌فرایندی (بی‌نیاز از root)
- مدل‌های اتلاف (رابط `LossModel` در `:24-26`):
  - `None` (`:29-31`)؛
  - `AllDrop` (`:35-37`)؛
  - `IID` (`:40-54`)؛
  - `TimeGE` (`:60-102`): Gilbert-Elliott زمان‌محور. فرمول میانگین اتلاف در `:58-59` آمده است.
- `Config` (`:105-113`) و `Relay` (`:118-133`): رله‌ای روی loopback. `scheduleLocked` (`:319-341`) همان ساعت گلوگاه و دورریختن از ته صف `netem` را تقلید می‌کند. پیش‌فرض صف 200 ms است (`:327-330`).
- `SetLoss` (`:174-183`) عوض‌کردن مدل اتلاف را وسط آزمون ممکن می‌کند. آمارها: `Stats`، `QueueDrops`، `LossFraction` (`:186-213`).
- مصرف‌کننده‌ها: `udpcarrier/lab_test.go` و `engine/carrier_udp_test.go` (بخش 10).

#### `probe/main.go` — آنچه کاربر حس می‌کند
- **سمت سرور (`:27-72`)** نقش پنل را بازی می‌کند. بایت اول هر اتصال حالت را تعیین می‌کند:
  - `'B'`: دانلود بی‌پایان با تکه‌های 64 KiB؛
  - `'U'`: چاهک بارگذاری؛
  - `'E'`: echo.
  همچنین UDP echo روی همان نشانی پاسخ می‌دهد (`:33-44`).
- **سمت کاربر (`:106-342`):**
  - `-bulk` اتصال دانلود و `-up` اتصال بارگذاری؛
  - echo یک‌بایتی هر `-every` (100 ms) روی **یک** اتصال ماندگار (`:216-256`)؛
  - هر ثانیه یک اتصال تازه و زمان «اتصال + بایت اول» (`:257-292`)؛
  - UDP echo اختیاری (`:293-327`)؛
  - `-seg` اتصال دانلود را پس از N بایت از نو می‌سازد (`:132-174`)؛
  - `-warm` (3 s) از آمار کنار گذاشته می‌شود (`:114`، `:128`).
- **خروجی:** یک خط JSON از نوع `result` (`:74-92`).

#### `dglab/main.go` — بار روی یک حامل datagram خام
- **چه می‌سنجد:** خودِ `udpcarrier` (Noise IKpsk2، ChaCha20-Poly1305، FEC تطبیقی، pacer) را روی هر encap اجرا می‌کند؛ **بدون** pool و engine. خروجی: goodput، اتلاف باقی‌مانده و تأخیر یک‌طرفه (هر دو فضای نام یک ساعت مشترک دارند).
- **عملیات قاب (`:32-37`):** `opData=0xd0`، `opStart=0xc0`، `opReport=0xe0`، `opStats=0xe1`.
- **سرور (`:161-235`):** شمارش بارگذاری را میان همهٔ پیوندهای یک اجرا مشترک نگه می‌دارد. با دریافت `opStart`، اگر بیت 1 روشن باشد، فرستندهٔ دانلود را راه می‌اندازد.
- **کلاینت (`:237-394`):**
  - `opStart` را سه بار تکرار می‌کند، چون قاب‌های کنترلی بدون FEC می‌روند (`:291-293`)؛
  - پس از پایان 1.5 s صبر می‌کند تا FEC و قاب‌های در راه برسند (`:317`)؛
  - آمار هر حامل را گزارش می‌کند (`:382-391`).
- **ردیابی:** `HS2_DGTRACE` هر ثانیه loss، parity، btlbw و rtt را چاپ می‌کند (`:295-304`).

#### `tcpload.py` — بار TCP با نرخ ثابت (برای `cpuquota.sh`)
- **`server`:** هر اتصال را با نرخ ثابت تغذیه می‌کند؛ گام 20 ms، تکه‌های 4096 بایتی (`tcpload.py:4-22`).
- **`client`:** N اتصال باز می‌کند و برای هر کدام این‌ها را ثبت می‌کند (`:23-46`):
  - کل بایت‌ها؛
  - بایت‌های پس از 10 s (`late`، `:38`)؛
  - بزرگ‌ترین فاصلهٔ میان دو دریافت (`maxgap`).

#### اسکریپت‌های یک‌آزمایشی
| اسکریپت | چه می‌آزماید | توپولوژی | خروجی |
|---|---|---|---|
| `run.sh` | حامل‌های جریانی (mtcp، l3mtcp، tls)، **فقط حالت direct** | `ir —veth— mid[netem] —veth— kh` (`:44-56`) | یک خط JSON (`:92-93`) |
| `dgtun.sh` | dgtun واقعی (pool + TUN) روی هر encap، direct یا reverse | همان سه فضای نام (`:42-56`) | یک خط JSON (`:117-118`) |
| `encap.sh` | فقط حامل خام با `dglab` | همان (`encap.sh:41-55`) | خط JSON `dglab` به‌اضافهٔ تنظیمات و آمار netem (`:66-68`) |
| `cpuquota.sh` | فرستندهٔ کم‌پردازنده؛ dgtun در حالت reverse روی icmp | **دو** فضای نام با veth مستقیم، بدون netem؛ در صورت نیاز tbf روی خروجی خارج (`:105-115`) | خلاصه + یک خط JSON (`:166-206`) |
| `transport-probe.sh` | **مسیر واقعی** میان دو سرور تولید؛ همهٔ حامل‌ها در حالت reverse | دو سرور واقعی، درگاه و رابط و زیرشبکهٔ جدا | جدول روی سمت ایران (`:213-235`) |
| `sweep.py` + `analyze.py` | اجرای موازی نقشهٔ آزمایش‌ها با `run.sh` و میانهٔ نتایج | — | `results.jsonl` و جدول میانه‌ها |

---

## 3. جریان داده و کنترل، گام‌به‌گام

### 3.1 یک آزمایش `run.sh` (پایهٔ جدول v2→v3 در README)
1. **محیط و پیش‌فرض‌ها (`run.sh:17-28`):** `MODE=mtcp`، `RATE=50mbit`، `DELAY=40ms` (یعنی RTT پایهٔ 80 ms)، `QUEUE=500ms`، `LOSS=0`، `FLOWRATE=0`، `BULK=8`، `UP=0`، `T=20s`، `UDP=0`، `MIN_LINKS=4`، `MAX_LINKS=16`. با `LAB_ID` نام فضاهای نام و رابط‌ها یکتا می‌شود و اجرای موازی ممکن است (`:19-24`).
2. **ساخت ابزار:** `netem` و `probe` **فقط اگر در `$W` نباشند** ساخته می‌شوند (`:38-39`). گواهی خودامضا با `CN=lab.example.com` (`:40-41`).
3. **شبکه:** veth با offloadهای خاموش (`ethtool -K … tso off gso off gro off tx off rx off`، `:50-54`). سپس netem فقط با `-rate -delay -queue -loss -flowrate` اجرا می‌شود (`:55-56`).
4. **پیکربندی‌ها (`:58-69`):**
   - کلید: 64 کاراکتر `ab`؛
   - خارج: `listen`، `backend_addr: builtin`، `expose: 127.0.0.1:5201`؛
   - ایران: `dial`، `forward_ports: 8443`، `min_links`/`max_links` از متغیرها، `mtu: 1380`؛
   - `IRAN_EXTRA`/`KHAREJ_EXTRA` فیلد اضافه تزریق می‌کنند.
5. **اجرا:** سرور `probe` در فضای خارج (`:70`)، سپس hs2 خارج، سپس hs2 ایران؛ هر دو با `HS2_NO_TUNE=1` (`:71-73`).
6. **انتظار:** تا 15 s برای باز شدن درگاه 8443، به‌اضافهٔ 2 s (`:75-79`).
7. **ping:** اگر `hs0` وجود دارد (l3mtcp و tls)، ping هر 0.2 s به `10.77.0.2` در طول آزمون (`:80-84`).
8. **سنجش:** `probe` کاربر به `127.0.0.1:8443` (`:85-86`).
9. **پایان:** پاک‌سازی و چاپ JSON با این فیلدها:
   - `ping_avg_max`؛
   - `ping_loss`؛
   - `link_churn`: شمارش خطوط لاگ ایران با الگوی `reap:|rebuilt|link down` (`:90`) — **مشاهده**: این الگو با قالب امروزی لاگ نمی‌خواند؛ بخش 13.

> نکتهٔ تأییدشده با کد: `HS2_NO_TUNE=1` فقط اعمال sysctlها را رد می‌کند. کنترل ازدحام پیوندها همچنان از طرح تنظیم (bbr) گرفته می‌شود، مگر `HS2_TUNE_CC` تنظیم شده باشد (`cmd/hs2/main.go:347-359`).

### 3.2 جاروب و تحلیل
1. **`sweep.py plan.json results.jsonl [--jobs N]`:** هر عضو plan مجموعه‌ای از متغیرهای محیطی برای `run.sh` است، با `label` و `rep` اختیاری (`sweep.py:2-10`).
   - اجراهای انجام‌شده با کلید JSON مرتب‌شده شناخته و رد می‌شوند، پس جاروب قابل ادامه است (`:14-15`، `:22-28`).
   - پیش‌فرض `--jobs 2` است (`:19`). توصیه: حداکثر نصف تعداد هسته‌ها، تا زمان‌بندی netem درست بماند (`:7-9`).
   - هر worker مقدار `LAB_ID=wid` را می‌گذارد (`:48`). مهلت هر اجرا 300 s است (`:49-50`).
   - خروجی هر اجرا `{"run":…, "result":…}` است؛ اگر نتیجه JSON نباشد، 500 نویسهٔ آخر خروجی ثبت می‌شود (`:51-56`).
2. **`analyze.py`:** بر اساس برچسب گروه‌بندی می‌کند و میانه می‌گیرد (`analyze.py:14-37`). ستون‌ها: Mbit، echo p50 و p95، conn p50، شمار شکست‌ها (`conn_fail + bulk_errors`)، میانگین ping روی hs0، و n.

### 3.3 `dgtun.sh`
- **پیش‌فرض‌ها (`:12-16`، `:23-26`):** `ENCAP=udp`، `RATE=50mbit`، `DELAY=20ms`، `QUEUE=300ms`، `BULK=4`، `T=12s`، `MIN=2`، `MAX=8`، `REVERSE=0`. اگر `BIN` داده نشود، hs2 با `CGO_ENABLED=0` ساخته می‌شود (`:37-40`).
- **پیکربندی (`:61-76`):**
  - هر دو سمت `carrier: dgtun` با `encap` و `proto`، `mtu: 1280`، `min_links`/`max_links`؛
  - در reverse، `addr` به نشانی ایران عوض می‌شود.
- **انتظار برای آماده‌شدن (`:88-95`):** هر دو `hs0` بالا باشند و در لاگ ایران ` up (now ` دیده شود (لاگ `engine/dgpool.go:717-719`)، و TCP به 8443 وصل شود. توضیح `:83-87`: در reverse، بدون این شرط حدود 2 s از pingها بی‌حامل می‌رفتند و 10 تا 22 درصد اتلاف نشان می‌دادند.
- **سنجش:** ping به `10.77.0.2`، سپس `probe` با `-seg` اختیاری.
- **`max_links` در خروجی** در واقع بیشترین `now N` دیده‌شده در لاگ است (`:115`)؛ یعنی بیشینهٔ شمار حامل‌ها، نه سقف پیکربندی.

### 3.4 `encap.sh` + `dglab`
- **پیش‌فرض‌ها (`encap.sh:14-18`):** `RATE=100mbit`، `QUEUE=200ms`، `LINKS=1`، `DIR=both`، `OFFER=0` (هر چه پذیرفته شود)، `SIZE=1200`، `T=8s`.
- netem همهٔ پرچم‌های اتلاف انفجاری را می‌گیرد (`:53-55`).
- سرور `dglab` روی `192.168.50.2:2096` و کلاینت با `-links/-dir/-rate/-size/-t` (`:58-62`).

### 3.5 `cpuquota.sh` (فاز X3)
- **هدف (`:2-26`):** بازسازی تکرارپذیر فرستندهٔ کم‌پردازنده.
  - `QUOTA` (پیش‌فرض 0.6 هسته، دوره 10 ms) سقف سخت CFS است و همهٔ نخ‌ها را با هم می‌خواباند.
  - `SHARE` تقسیم وزنی میان hs2 و حلقه‌های مشغول در دو cgroup است؛ نخ‌ها را یکی‌یکی کنار می‌گذارد، مثل سروری که با تونل دیگری شریک است (Phase Y نشان داد این دو رژیم نتیجهٔ متفاوت می‌دهند).
- **جداسازی پردازنده (`:105-112`):**
  - hs2 خارج روی `CPUS_K=0,1`؛
  - hs2 ایران و تولیدکنندهٔ بار و ping روی `CPUS_I=2,3`؛
  - کار دریافت veth ایران هم با RPS به `CPUS_I` می‌رود.
- **پیکربندی (`:118-123`):** reverse، `forward_ports` خالی (ترافیک مستقیم روی IPهای TUN)، `min_links = max_links = LINKS` (پیش‌فرض 4)، و `HS2_PPROF=127.0.0.1:30421` روی سمت خارج.
- **اندازه‌گیری‌ها (`:145-206`):**
  - CPU فرایند خارج: `utime + stime` از `/proc/pid/stat` تقسیم بر 100؛
  - `OutDiscards`؛
  - `nr_throttled`؛
  - ping بی‌بار (40 تا با فاصلهٔ 0.05 s) و ping زیر بار (از ثانیهٔ 5)؛
  - هر 6 s یک `hs2 status`؛
  - سهم پرچم‌های `S` و `P` در مدخل‌های `carriers`؛
  - میانگین `writers waited for pacer room N%`.
- **خروجی‌ها:** `mbit`، `mbit_late` (پس از 10 s)، `cpu_s`، `cpu_cores`، `sys_share`، `mbit_per_cpu_s`، `failed_conns`، `max_gap_s`، `throttled_periods`، `out_discards`، `share_S`، `share_P`، `send_held_pct_mean`، `ping_idle`، `ping_load`.
- `TRACE=1` در ثانیهٔ 12 یک ردگیری اجرای 5 ثانیه‌ای از hs2 خارج می‌گیرد (`:159-161`).

### 3.6 `transport-probe.sh` (مسیر واقعی، بی‌آسیب به تونل زنده)
- **اجرا:** هم‌زمان روی دو سرور، با `ROLE` و `PEER` و `PASS` یکسان (`:2-18`). کلید = sha256 از PASS (`:37`).
- **جدا از تونل زنده:** درگاه `TPORT=3390`، رابط `hst9`، زیرشبکهٔ `10.79.0.x`، echo روی 19000، forward روی 18443 (`:35-36`).
- **فهرست ده حامل (`:46-57`):** `auto`، `udp`، `mtcp`، `tls`، `tun-udp/icmp/gre/ipip/ipx` (dgtun)، `tun-tcp` (l3mtcp). همه با `reverse: true` و pool به صورت `min 2 / max 8 / per_link 8` (`:141`).
- **همگام‌سازی:** دو سمت با ساعت دیواری قدم‌به‌قدم جلو می‌روند؛ شکاف `slot = now / SLOT` (`:179-205`). سمت ایران از یک‌سوم هر شکاف به بعد با `probe_once` می‌سنجد (`:92-134`):
  - اتصال را تا پایان بودجه تکرار می‌کند؛
  - echo درست را بررسی می‌کند؛
  - 5 s با بافر 32 KiB می‌فرستد و بازگشت echo را می‌شمارد؛ نرخ گزارش‌شده بر پایهٔ بایت‌های برگشتی است.
- **دو مسیر سنجش:** `auto` و `udp` درگاه‌گردان ندارند، پس مستقیم به `KH_TUN:19000` سنجیده می‌شوند (`:44-45`، `:192`).
- **نتیجه:** بهترین نتیجهٔ هر حامل در میان دورها نگه داشته می‌شود (`:195-202`). جدول نهایی «yes/NO» است با دلیل (`:213-235`).

### 3.7 مسیر هر قاب در `netem`
`Recvfrom` ← نادیده گرفتن قاب خروجی ← `dropUDP` ← `allow` ← `loss/GE` ← پلیس‌گر مقصد ← پلیس‌گر جریان ← ساعت گلوگاه (اگر تأخیر صف بیش از `queue` شود، `qdrop`) ← تأخیر و jitter ← کانال 65536‌تایی ← heap نویسنده بر پایهٔ زمان تحویل ← `Sendto` (`netem/main.go:207-352`).

### 3.8 اعتبارسنجی میدانی (VALIDATION.md)
1. **پیش‌نیاز:** هر دو سرور یک build دارند؛ شناسهٔ انتشار `da621f5db2bb` (`VALIDATION.md:279-287`).
2. **پاییدن لاگ سمت ایران** با `journalctl … | grep -E 'stuck|answer promptly|degraded|closed with its|path is lossy'` (`:39`)، و شمارش ساعتی پنج الگو (`:51-59`).
3. **سناریوها:**
   - V1 پیوسته، 24 ساعت با اوج عصر؛
   - V2 تا V4 در ساعت خلوت:
     - V2: کندکردن یک پیوند با iptables `limit 3/s`؛
     - V3: سه یا چهار پیوند؛
     - V4: قطعی 40 s؛
   - V5 تا V8: پرسش باز اتلاف پس از ازدحام، پهنای باند بالا، خوانندهٔ گیرکرده، پایین‌آمدن pool؛
   - V9 تا V14: icmp و datagram (قطع یک حامل، ping زیر بار، CPU بر گیگابایت، سهم گلوگاه، فرستندهٔ کم‌پردازنده، استتار).
4. **شرط معتبر بودن V11 تا V13:** فرستنده نباید اشباع باشد؛ زیر 75٪ مشغول و زیر 20٪ انتظار برای هسته (`:170-182`). pool با `max_links 4` و سپس `min_links 4` ثابت شود (`:184-189`).
5. **معیار برگشت (`:27-31`):**
   - قطع موجی کاربران؛
   - بیش از چند خط `stuck:` در ساعت بی‌دلیل؛
   - خطوط `stuck:` هم‌ثانیه، بیش از یک‌هشتم pool؛
   - ده‌ها خط `degraded (up-loss` پس از ازدحام؛
   - FAIL تازه در doctor.

---

## 4. جدول ثابت‌ها و آستانه‌ها

### 4.1 عددهایی که مستندات می‌گویند، در برابر کد
| نام / ادعا | مقدار در مستند | مقدار در کد | مکان کد | وضعیت |
|---|---|---|---|---|
| کمینهٔ پیوندها | 2 (`README.md:24`) | `min=2` اگر صفر باشد | `cmd/hs2/main.go:646-648` | تأیید |
| `per_link` | 8 (`README.md:29`) | 8 | `cmd/hs2/main.go:653-655` | تأیید |
| آغاز گرم | 8 پیوند (`README.md:92`) | `warmStartLinks = 8` | `engine/health.go:85` | تأیید |
| شروع گرم پس از راه‌اندازی دوباره | 15 دقیقه؛ نوشتن پس از 1 دقیقه کارکرد (`README.md:185-189`) | `warmMaxAge = 15m`، `warmAfter = 1m` | `cmd/hs2/status.go:201`، `:236` | تأیید؛ نام فایل در عمل `/run/hs2/<مسیر مطلق با - به‌جای />.warm` است (`:193-197`) |
| تیک سلامت | 2 s (`BUILD.md:80`) | `healthTick = 2s` | `engine/health.go:18` | تأیید |
| گام probe | حدود 25٪ (50٪ در زنجیرهٔ موفق) (`README.md:35`؛ `BUILD.md:87-91`) | `probeStepMax=32`، `probeChainStepMax=64` | `engine/autopilot.go:301-307` | سقف‌های گام در مستند نیامده‌اند |
| عقب‌نشینی probe | تا 8 دقیقه (`README.md:38`) | `backoffMax = 8m` (پایه 30 s) | `engine/autopilot.go:142-143` | تأیید |
| کوچک‌شدن | پس از 60 s، هر 30 s یک گام، با 70٪ ظرفیت (`BUILD.md:97-99`) | `shrinkDwell=60s`، `shrinkStep=30s`، `util=0.7` | `engine/autopilot.go:151-154` | تأیید |
| نگه‌داشت پس از برگرداندن کوچک‌شدن | 10 دقیقه، دوبرابرشونده (`BUILD.md:100-101`) | `holdBase=10m`، `holdMax=2h` | `engine/autopilot.go:157-158` | تأیید؛ سقف 2 h در مستند نیست |
| `drain_idle_sec` | 310 s (`README.md:44`) | `drainIdleDefault = 310s` | `engine/linkmanager.go:78` | تأیید |
| بستن اتصال‌های کم‌حرکت روی پیوند در حال بازنشستگی | در README نیامده | `retireForce = 20m` | `engine/linkmanager.go:87` | **مستند نشده** |
| آستانهٔ پیوند پراتلاف | بیش از 12٪، سه نمونه (`README.md:46`؛ `VALIDATION.md:48`) | `lossFrac=0.12`، `degradeStreak=3` | `engine/health.go:31-35` | تأیید |
| تخلیهٔ پیوند degraded | 45 s، سکون 15 s، سپس 45 s دیگر (`README.md:49-55`) | `maxDrain=45s`، `drainStall=15s`، `maxDrainActive=90s` | `engine/health.go:76-78` | تأیید |
| سقف تخلیهٔ هم‌زمان | «حداکثر یک‌هشتم pool» (`README.md:64-65`، `:84-85`) | `max(2, ceil(n/8))` | `engine/linkmanager.go:1253` | تقریباً؛ در poolهای زیر 16 پیوند کمینه 2 است، یعنی بیش از یک‌هشتم |
| پیوند گیرکرده (stuck) | انتظار 6 s، کمتر از 6 KB/s، کندی 4 برابر و بیش از 0.5 s، بازیابی 30 s تا 2 min (`README.md:73-82`) | `stuckWait=6s`، `stuckStreak=2`، `stuckMoveFloor=12 KiB` در هر تیک 2 ثانیه‌ای، `stuckInflate=4`، `stuckInflateFloor=500ms`، `stuckRecover=30s`، `stuckRecoverMax=2m`، `stuckBaseMins=10` | `engine/stuck.go:61-95` | تأیید |
| پیوند مشکوک | 12 s (`BUILD.md:107-109`) | `suspectAfter = 12s` | `engine/linkmanager.go:317` | تأیید |
| تشخیص پیوند مرده | «حدود دو ثانیه» (`README.md:18-19`؛ `hs2-src/README.md:18-19`) | تیک 2 s فقط بسته‌شدن صریح (`Alive()` در `engine/mtcp_link.go:190-195`) را می‌بیند. برای مسیر سیاه‌چاله: مشکوک پس از 12 s، `TCP_USER_TIMEOUT=20s` (`tlscarrier/tune_linux.go:22`)، `KeepAliveTimeout=24s` (`engine/mtcp_link.go:279`) | — | **ناسازگار جزئی** (بخش 13) |
| دروازهٔ شماره‌گیری | حداکثر 8 در جریان، فاصلهٔ 40 تا 160 ms، حدود 10 در ثانیه (`README.md:190-194`) | `gateInflight=8`، `gatePerSec=10`، `jitterGap` | `engine/dialgate.go:19-26` | تأیید |
| نگه‌داشت refill | 10 s، 3 s بی‌پیوند تازه، نمایش 5 min (`README.md:202-217`) | `refillHoldMax=10s`، `refillStall=3s`، `refillKeep=5m`، `refillRearm=1m` | `engine/refill.go:49-59` | تأیید؛ `refillRearm` در README نیست |
| خوانندهٔ گیرکرده | 6 s (`README.md:221`) | `stuckFor = 6s`، `guardTick=2s` | `engine/wedge.go:55`، `:62` | تأیید |
| کانال جانبی l3mtcp | صف 60 ms؛ جابه‌جایی پس از 12 s (`README.md:234-235`، `:662`) | `l3MaxSojourn=60ms`، `l3SessionSilent=12s`، `l3QueueLen=256`، `l3StreamDeadAfter=30s`، `l3QuietKeepalive=10s` | `engine/l3_link.go:53-87` | تأیید |
| صف هر جریان UDP | 256 datagram، 512 KB (`README.md:245-247`) | `udpFlowQueue=256`، `udpFlowBytes=512<<10` | `engine/stream_iran.go:287-288` | تأیید |
| بافرهای smux | 8 MiB برای نشست، حدود 12 MiB با گردکردن (`README.md:129-132`) | `SmuxSessionBuffer=8 MiB`، `SmuxStreamBuffer=2 MiB`، `SmuxFrameSize=16 KiB`، `LinkWorstCaseMiB=12` | `engine/mtcp_link.go:255-265`؛ `tune/tune.go:203-210` | تأیید |
| keepalive در smux | هر 4 تا 8 s (`README.md:174`) | `4000+rand(4000)` ms | `engine/mtcp_link.go:278` | تأیید |
| پینگ کنترلی | هر 3 s روی پیوندهای فعال، کمتر روی بی‌کارها (`README.md:175`) | `controlInterval=3s`؛ پیوند بی‌کار هر تیک 3 تا 5 (حدود 9 تا 15 s)؛ پیوند سبک 2 تا 4 s | `engine/health.go:96`؛ `engine/control.go:92-131` | تأیید |
| `TCP_NOTSENT_LOWAT` | 32 KiB (`README.md:671`) | `NotSentLowat = 32<<10` | `tlscarrier/tune_linux.go:19` | تأیید |
| قاب جریان | 16 KiB (`README.md:671`) | `SmuxFrameSize = 16<<10` | `engine/mtcp_link.go:258` | تأیید |
| سقف خودکار | یک پیوند برای هر 48 MB، حداکثر 300، کف 32/48/64، 2 تا 3 هسته حداکثر 128 (`README.md:115`) | `LinkRAMPerLinkMB=48`، `MaxLinksCap=300`، `maxLinksFewCores=128`، پروفایل 32/48/64 | `tune/tune.go:198-251` | تأیید (مثال‌های 16 GB/4 هسته = 300 و 2 GB/2 هسته = 48 حساب شد) |
| `max_links` حذف‌شده از پیکربندی | 32 (`README.md:117`) | `legacyMaxLinks = 32` | `cmd/hs2/main.go:686` | تأیید |
| سقف icmp | 8 (`README.md:617-620`) | `icmpMaxLinks = 8` | `cmd/hs2/main.go:678` | تأیید |
| هشدار `min_links` | بیش از 64 (`README.md:163-164`) | `minLinksHigh = 64`؛ هشدار بالای 1024؛ خطا بالای 65535 | `cmd/hs2/check.go:452-477` | تأیید |
| مجموع چند تونل در doctor | هشدار بالای 40٪ RAM (`README.md:142-144`) | `pct > 40` | `cmd/hs2/doctor.go:691` | تأیید |
| GOMEMLIMIT | نصف RAM (`README.md:135-136`) | `ramMB<<20/2` | `cmd/hs2/main.go:309-320` | تأیید |
| datagram: صف و ماندگاری | 256، 50 ms (`README.md:403-405`) | `dgQueueLen=256`، `dgSojourn=50ms` | `engine/dgpool.go:49-50` | تأیید |
| حامل بی‌صدا (mute) و دیدبان | 1 s، 3 s، دیدبان هر 5 s، انتقال اجباری 30 s (`README.md:236-240`، `:543-555`) | `dgMuteAfter=1s`، `dgSilentDead=3s`، `dgScoutEvery=5s`، `dgRetireForce=30s`، `feedbackEvery=100ms` | `engine/dgpool.go:60-74`، `:347`؛ `udpcarrier/carrier.go:54-57` | تأیید |
| پلیس‌گر: آزمون | دو رویداد در 30 s، سقف 90٪ (`README.md:386-387`) | `govDetectWindow=30s`، `govCapFrac=0.9` | `udpcarrier/governor.go:119-120` | تأیید |
| پلیس‌گر: بازکاوش پس از تأیید | «5٪ هر 10 s» (`README.md:389`) | `govRaiseEvery=4s`، `govRaiseGain=1.15` | `udpcarrier/governor.go:125-126`، `:470-471` | **ناسازگار** |
| پلیس‌گر: استراحت پس از برداشتن سقف | 5، 10، 20 … تا 1 h (`README.md:393-395`) | `govRest=5m`، با شیفت حداکثر 4 و سقف `govRestMax=1h` | `udpcarrier/governor.go:483-487` | تأیید |
| جریان تعاملی در صف منصفانه | کمتر از 256 kbit/s (`README.md:474-475`) | `fqSparseRate = 256e3/8` | `engine/dgfq.go:62` | تأیید |
| اعتبار دیرکرد pacer | 10 ms به‌جای 2 ms؛ بیشتر هنگام اشباع (`README.md:503-506`، `:446`) | `pacerQuantum=2ms`، `pacerLateCredit=10ms`، `pacerSatCredit=50ms` | `udpcarrier/pacer.go:92-120` | تأیید |
| استتار icmp در حالت بی‌کار | «ساکت، keepalive حدود 5 s» (`README.md:589-590`) | `camoIdlePoll = 700ms` با jitter؛ کاملاً ساکت نمی‌شود (CA1b) | `udpcarrier/carrier.go:22-45` | **ناسازگار** |
| انتظار نصب‌کننده برای «آماده» | تا 40 s (`README.md:627`) | `HS2_VERIFY_SECS` با پیش‌فرض 60 | `install.sh:902`، `:4173`، `:973-987` | **ناسازگار** |
| پایش گواهی | «ظرف یک دقیقه» (`README.md:277`) | `time.NewTicker(time.Minute)` | `cmd/hs2/cert.go:119` | تأیید |
| نسخهٔ پشتیبان | 10 تای آخر (`README.md:653-654`) | `BACKUP_KEEP=${HS2_KEEP_BACKUPS:-10}` | `install.sh:3907` | تأیید |
| نسخهٔ Go | 1.27 به بالا (`BUILD.md:3`) | `go 1.27` | `hs2-src/go.mod:3` | تأیید (Go محیط این نشست 1.24.7 است؛ ساخت محلی بدون دریافت ابزار نسخهٔ بالاتر ممکن نیست) |

### 4.2 پیش‌فرض‌های ابزارهای آزمایشگاه
| ابزار | نام | مقدار | path:line | معنی |
|---|---|---|---|---|
| run.sh | `RATE/DELAY/QUEUE` | 50mbit / 40ms / 500ms | `run.sh:26` | گلوگاه، تأخیر یک‌طرفه، صف |
| run.sh | `BULK/T` | 8 / 20s | `run.sh:27` | دانلودهای موازی، مدت |
| run.sh | `MIN_LINKS/MAX_LINKS` | 4 / 16 | `run.sh:68` | پاکت pool در آزمایش |
| run.sh | `mtu` | 1380 | `run.sh:61`، `:67` | MTU رابط hs0 |
| run.sh | انتظار برای درگاه | 60×0.25 s | `run.sh:75-78` | حدود 15 s |
| dgtun.sh | `RATE/DELAY/QUEUE/T/BULK/MIN/MAX` | 50mbit/20ms/300ms/12s/4/2/8 | `dgtun.sh:24-26` | — |
| dgtun.sh | انتظار حامل | 80×0.25 s | `dgtun.sh:89-95` | حدود 20 s |
| encap.sh | `RATE/QUEUE/LINKS/SIZE/T` | 100mbit/200ms/1/1200/8s | `encap.sh:26-29` | — |
| cpuquota.sh | `QUOTA/PERIOD_US/LINKS/CONNS/DUR/CONNRATE` | 0.6/10000/4/8/30/100000000 | `cpuquota.sh:45-48` | هستهٔ مجاز، دورهٔ CFS، حامل‌ها، دانلودها، ثانیه، بایت بر ثانیه برای هر دانلود |
| cpuquota.sh | `CPUS_K/CPUS_I` | 0,1 / 2,3 | `cpuquota.sh:47` | جداسازی پردازنده |
| transport-probe.sh | `TPORT/SLOT/ROUNDS` | 3390 / 35 / 2 | `transport-probe.sh:22` | درگاه آزمون، ثانیه در هر شکاف، دورها |
| transport-probe.sh | pool | min 2, max 8, per_link 8 | `transport-probe.sh:141` | — |
| sweep.py | `--jobs` و مهلت هر اجرا | 2 / 300 s | `sweep.py:19`، `:50` | — |
| probe | `-every/-warm/-t/-bulk` | 100ms / 3s / 20s / 8 | `probe/main.go:110-114` | فاصلهٔ echo، گرم‌شدن، مدت، دانلودها |
| probe | مهلت echo / اتصال تازه / UDP | 5 s / 5 s / 2 s | `probe/main.go:240`، `:271-274`، `:312` | — |
| netem | `-queue/-goodms/-flowburst/-dstburst/-dstpenalty/-dstpenloss` | 200ms/150/64KiB/5s/1s/0.8 | `netem/main.go:391-403` | — |
| netem | بافر سوکت | 8 MiB | `netem/main.go:146-147` | SO_RCVBUF و SO_SNDBUF |
| netsim | صف پیش‌فرض | 200 ms | `netsim/netsim.go:327-330` | — |
| dglab | تکرار `opStart` / تخلیهٔ پایانی | 3 بار / 1.5 s | `dglab/main.go:291-293`، `:317` | — |

---

## 5. حلقه‌های کنترلی (آنچه مستندات وعده می‌دهند و آنچه آزمایشگاه می‌گرداند)

| حلقه | ورودی | شرط | خروجی | دوره | مرجع |
|---|---|---|---|---|---|
| autopilot | نمونهٔ سلامت هر پیوند: نرخ، جریان‌های فعال، تحت فشار بودن | کف `ceil(active/per_link)`؛ رشد فقط وقتی پیوندهای تحت فشار جای آزاد نگذاشته‌اند؛ کوچک‌شدن پس از 60 s | هدف شمار پیوندهای در حال خدمت | 2 s | `BUILD.md:73-102`؛ `engine/autopilot.go:11-44` |
| قاعدهٔ اتلاف | بازارسال‌ها میان دو pong (دانلود) یا TCP_INFO (بارگذاری) | بیش از 12٪، سه نمونه، و کمتر از نصف نرخ پیوندهای تحت فشار | پیوند degraded و تخلیه | هر pong (3 s) یا تیک | `README.md:46-68`؛ `engine/loss.go` |
| قاعدهٔ گیرکردن | عمر قدیمی‌ترین پینگ بی‌پاسخ | 6 s در دو نمونه و جابه‌جایی کم | تخلیهٔ فوری اتصال‌های ساکن | 2 s | `README.md:69-90`؛ `engine/stuck.go` |
| نگهبان خوانندهٔ گیرکرده | نوشتن محلی ناتمام | 6 s، با پرشدن بافر پیوند یا فشار حافظهٔ TCP | ریست همان اتصال‌ها | 2 s | `README.md:218-233`؛ `engine/wedge.go:53-62` |
| refill | رسیدن پیوندها پس از شروع یا قطعی کامل | زیر سهم منصفانه | صف انتظار اتصال‌های تازه | 100 ms | `engine/refill.go:49-55` |
| پلیس‌گر datagram (governor) | اتلاف و نرخ و صف همهٔ pool | رویدادهای هم‌زمان بدون صف | سقف کل pool | 500 ms | `README.md:378-399`؛ `udpcarrier/governor.go:109-132` |
| mute | شنیده‌نشدن هیچ بسته | 1 s سکوت، در حالی که حامل دیگری می‌شنود | جابه‌جایی جریان‌ها؛ بستن در 3 s | 250 ms | `engine/dgpool.go:74-80` |
| شکاف‌های `transport-probe` | ساعت دیواری | `now/SLOT` عوض شود | اجرای حامل بعدی | 2 s | `transport-probe.sh:179-205` |
| echo در `probe` | تیکر | — | RTT بایت echo | 100 ms | `probe/main.go:231-255` |
| اتصال تازه در `probe` | تیکر | — | زمان اتصال + بایت اول | 1 s | `probe/main.go:258-292` |
| نمونهٔ وضعیت در `cpuquota` | — | — | `st$s.txt` | 6 s | `cpuquota.sh:156-158` |

---

## 6. حالت‌ها و گذارها، خطاها و بازیابی (از دید مستندات و آزمایشگاه)

- **حالت‌های پیوند جریانی** (README و BUILD):
  - serving: می‌پذیرد؛
  - retiring: بازنشسته، با پایان اتصال‌ها بسته می‌شود؛
  - degraded یا stuck: تخلیه؛
  - suspect: 12 s سکوت، تا رسیدن بسته‌ای دوباره در حال خدمت به حساب نمی‌آید.

  گذار بازگشتی: «retiring به serving» وقتی هدف بالا برود (`BUILD.md:104-105`). مرجع کد: `engine/linkmanager.go:300` (`serving` = نه retiring، نه degraded، نه draining، زنده و نه suspect).
- **فازهای autopilot:** `steady/scaling/probing/holding/shrinking` (`engine/autopilot.go:170-176`). در خط `links:` وضعیت نمایش داده می‌شوند (`README.md:98-107`؛ `cmd/hs2/status.go:547-570`).
- **پلیس‌گر datagram:** عادی ← آزمون (سقف 90٪) ← تأیید (نگه‌داشت، parity بر پایهٔ اتلاف خود مسیر) یا برداشتن سقف (استراحت 5 min تا 1 h) (`README.md:386-395`).
- **حامل datagram:** زنده ← mute (1 s) ← بسته (3 s) ← جایگزین. اگر هیچ حاملی نشنود، فقط دیدبان هر 5 s.
- **برگشت میدانی (VALIDATION):** نصب دوبارهٔ باینری سنجاق‌شده به ثبت قبلی با `HS2_REPO_RAW=…/9b74b3c` (`VALIDATION.md:17-25`). برای main: revert، بدون force-push.
- **خطا در ابزارهای آزمایشگاه:**
  - `sweep.py`: خروجی غیر JSON به صورت `{"error": …}` ثبت می‌شود؛ مهلت 300 s با استثنای `TimeoutExpired`. **مشاهده**: این استثنا گرفته نمی‌شود، پس worker همان‌جا می‌میرد (`sweep.py:49-56`).
  - `transport-probe.sh`: دلیل‌های FAIL: `no-connect`، `no-data`، `bad-echo`، `not-tested`.
  - `cpuquota.sh`: اگر hs2 خارج بالا نیاید یا انتقال به cgroup شکست بخورد، خروج با خطا (`:133`، `:138`).
  - `dglab`: اگر شماره‌گیری شکست بخورد، `{"encap":…,"error":…}` (`dglab/main.go:247-250`).

---

## 7. قالب پیام‌ها و خروجی‌ها

### 7.1 پروتکل ابزارها
- **`probe`:** یک بایت فرمان در آغاز هر اتصال TCP: `B` دانلود، `U` بارگذاری، `E` echo. سرور برای `E` همان بایت را برمی‌گرداند و بعد `io.Copy` می‌کند (`probe/main.go:57-69`). UDP echo یک رشتهٔ 8 رقمی است (`:313`).
- **`dglab`** (محتوای `core.TypeData`؛ `dglab/main.go:31-37`):
  - `opData`: `[0xd0][seq:8][sendNanos:8][pad]`
  - `opStart`: `[0xc0][flags:1][rateKbps:4][size:2][durMs:4][run:4]`
    - بیت 0 پرچم: دانلود بخواه؛
    - بیت‌های 2 به بالا: شمارهٔ پیوند. هر پیوند فضای شماره‌گذاری جدا دارد، با `<<56` (`:211`).
  - `opReport`: `[0xe0]`
  - `opStats`: `[0xe1][json]` با `delivered`، `bytes`، `first`، `last`، `down_sent`، `p50`، `p95`، `p99`.

### 7.2 طرح‌وارهٔ JSON خروجی‌ها
- **`probe`:** `mbps`، `up_mbps`، `echo_n`، `echo_lost`، `echo_p50_ms`، `echo_p95_ms`، `echo_p99_ms`، `echo_max_ms`، `conn_n`، `conn_fail`، `conn_p50_ms`، `conn_p95_ms`، `udp_n`، `udp_lost`، `udp_p50_ms`، `udp_p95_ms`، `bulk_errors` (`probe/main.go:74-92`).
- **`run.sh`:** `mode`، `rate`، `delay`، `loss`، `flowrate`، `bulk`، `ping_avg_max` («avg/max»)، `ping_loss`، `link_churn`، `probe` (`run.sh:92-93`).
- **`dgtun.sh`:** `encap`، `reverse`، `rate`، `delay`، `loss`، `tun_up`، `ping_avg_max`، `ping_loss`، `max_links` (بیشینهٔ دیده‌شده)، `probe` (`dgtun.sh:117-118`).
- **`encap.sh`:** `path{…}`، `netem` (رشتهٔ آمار)، `run` (خروجی dglab: `encap`، `links`، `dir`، `rate_mbps`، `size`، `t`، `setup_ms`، `up/down{sent, delivered, residual_loss_pct, goodput_mbps, p50_ms, p95_ms, p99_ms}`، و برای هر حامل `carrier[]{loss_ppm, parity, btlbw_mbps, rtt_ms, send_mbps, queue_ms, pacer_held_ms, pacer_write_ms, pacer_writes}`) (`dglab/main.go:70-78`، `:318-393`).
- **`cpuquota.sh`:** فهرست در بخش 3.5 (`cpuquota.sh:195-202`).

### 7.3 قالب‌هایی که مستندات توضیح می‌دهند و کد دارد
- **خط `links:`:** `7 up = 5 serving + 2 retiring / target 5 (shrinking, range 2–32)` (`README.md:104`؛ `cmd/hs2/status.go:547-570`)؛ در reverse ممکن است `, capped at N by the Kharej server` اضافه شود.
- **مدخل `carriers`:** `id:state:sent/loss% rRATE/bwBTLBW FLAGS sSENT qQUEUE/SRTT` (`README.md:412-419`؛ کد: `engine/dgpool.go:1926-1929`). پرچم‌ها `P/S/C/M` یا `-`. بالای 32 حامل فقط شمارش و 10 حامل پراتلاف (`carrierLineMax=32`، `carrierLineWorst=10`، `engine/dgpool.go:1941-1944`). الگوی `cpuquota.sh:192` با این قالب جور است.
- **خط `sending:`:** `pacers sent … · writers waited for pacer room N% of the time · fair-queue wait … · socket write … µs (… datagram(s) each)` (`cmd/hs2/status.go:885-891`). الگوی `pacer room (\d+)%` در `cpuquota.sh:194` جور است.
- **فیلدهای فایل وضعیت** که README فهرست کرده (`README.md:405-412`، `:427-434`، `:454-459`) در `cmd/hs2/status.go` تعریف شده‌اند (مثلاً `fec_recovered`، `:118`).
- **انواع جریان smux** (برای ارجاع؛ `BUILD.md:82-84`، `:111-113`): `kindTCP=1`، `kindUDP=2`، `kindL3=3`، `kindCtrl=4`، `kindPool=5`، `kindStats=6`، `kindInfo=7`، `kindTCPPort=8`، `kindUDPPort=9` (`engine/stream.go:32-48`).

---

## 8. متن دقیق لاگ‌های مهم که مستندات به آن تکیه دارند

| لاگ (کوتاه‌شده) | معنی | مستند | کد |
|---|---|---|---|
| `link N stuck: its traffic has waited %s for an answer while it moved %s in %s (the other links answer in ~%dms) — draining` | پیوند با گلوگاه چند بسته در ثانیه کند شده؛ کاربرانش جابه‌جا می‌شوند | `README.md:85-86`؛ `VALIDATION.md:44` | `engine/linkmanager.go:1988` |
| `… stuck — its connections that moved no data for %s are closed now …` | گام تخلیهٔ همان پیوند | `VALIDATION.md:45` | `engine/linkmanager.go:1228` |
| `N of M busy links have waited %s+ for an answer and only K answer promptly — the path or the other server is slow, not those links: none is drained` | کندی کل مسیر؛ هیچ پیوندی تخلیه نمی‌شود | `VALIDATION.md:46` | `engine/linkmanager.go:1961` |
| `… the K that answer promptly take ~%dms, %.0f× their usual ~%dms — the path is congested …` | ازدحام مسیر (Q8) | `VALIDATION.md:47` | `engine/linkmanager.go:1964` |
| `link %d degraded (up-loss %v, down-loss %v%s, rtt %dms) — draining` | قاعدهٔ اتلاف | `README.md:65-67`؛ `VALIDATION.md:48` | `engine/loss.go:221` |
| `%d of %d busy links resend more than %.0f%% — the path is lossy, not those links: none is drained` | اتلاف کل مسیر | `README.md:67-68`؛ `VALIDATION.md:49` | `engine/loss.go:202` |
| `… degraded for %s — closed with its %d remaining connection(s) …` | بسته‌شدن پیوند degraded با بقیهٔ اتصال‌ها | `VALIDATION.md:39` (الگوی `closed with its`) | `engine/linkmanager.go:1223` |
| `mtcp: reset %d connection(s) on %d link(s) whose app had taken nothing for %s …` | نگهبان خوانندهٔ گیرکرده (بافر پر) | `README.md:221-222` | `engine/wedge.go:272` |
| `mtcp: reset %d connection(s) whose app had taken nothing for %s while kernel TCP memory was above its pressure mark …` | همان، هنگام فشار حافظهٔ TCP | `README.md:229-231`؛ `VALIDATION.md:130-132` | `engine/wedge.go:267` |
| `kernel TCP memory on %s is above its pressure mark — … none is judged …` | هیچ پیوندی داوری نمی‌شود | `README.md:232` | `engine/linkmanager.go:1957` |
| `mtcp: no link up to the edge — dials fail (%v); one slot keeps trying …` و `mtcp: a link to the edge is back after %s with none up (%d dial(s) failed meanwhile) …` | آغاز و پایان قطعی از دید خارج | `README.md:199-201` | `engine/exit_pool.go:267`، `:312` |
| `refill: …` و `links open at the dial pace … — expected, not a fault` | نگه‌داشت refill | `README.md:213-216` | `engine/refill.go:183`، `:282`، `:332-345` |
| `link pool: coming up at %d links, the size it had before this restart …` | آغاز گرم | `README.md:187-188` | `cmd/hs2/main.go:291` |
| `link pool: ceiling N links — …` | سقف و دلیلش | `CHANGELOG.md:470-472` | `cmd/hs2/main.go:729-753` |
| `l3: dropped %d packets in 30s on the tun side channel (queue limit or no link) — …` | دورریختن روی کانال جانبی l3mtcp | `README.md:663-664` | `engine/l3_link.go:389` |
| `dg: carrier %d %s up (now %d)` | حامل datagram بالا آمد؛ `dgtun.sh` با آن آماده‌بودن را تشخیص می‌دهد | `dgtun.sh:91`، `:115` | `engine/dgpool.go:717-719` |
| `dg: carrier N has heard nothing … for 1.2s while … still do — …` و `… heard nothing for 3.2s — closed; a new carrier replaces it` | حامل بی‌صدا | `README.md:550-552`؛ `VALIDATION.md:154-157` | `engine/dgpool.go` (mute) |
| `dg: policer confirmed: …`، `… lowered to %.1f Mbit/s`، `… cap lifted (detection rests %s)` | پلیس‌گر | `README.md:397-399` | `udpcarrier/governor.go:428`، `:467`، `:492` |
| `cpu: the server is saturated — …` | اشباع کل سرور | `README.md:457-459` | `cmd/hs2/hostcpu.go:230` |
| `encap icmp: carriers send through a send-only raw socket … (HS2_RAW_TX=0 turns it off)` | سوکت فقط‌ارسال icmp | `README.md:532-541` | `encap/rawtx_linux.go:153` |
| `tun %s up: … (datagram pool, encap %s, TCP offload on)` | خط آغاز dgtun | `README.md:522-529`؛ `VALIDATION.md:192-193` | `cmd/hs2/main.go:851` |
| `tuning: HS2_TUNE_…=…` | بازنویسی تنظیم آزمایشگاهی | `BUILD.md:202-204` | `cmd/hs2/main.go:262-274` |

---

## 9. گزینه‌های پیکربندی و متغیرهای محیطی

### 9.1 کلیدهای پیکربندی که برای کاربر مستند شده‌اند
(ساختار کامل: `cmd/hs2/main.go:36-105`)

| کلید | معنی | مستند |
|---|---|---|
| `mode` | `dial` (ایران، لبه) یا `listen` (خارج، خروجی) | `run.sh:60`، `:66`؛ ضمنی در README |
| `carrier` | `mtcp`، `l3mtcp` (و `l3`)، `tls`، `udp`، `auto`، `dgtun`، `noise`، `reality` | `README.md:751-752`، `:754-767`؛ `BUILD.md:138-156` |
| `reverse` | چه کسی شماره می‌گیرد | `README.md:147-157`؛ `UDP REPORT §Direct vs Reverse` |
| `encap`، `proto` | encap در dgtun، پروتکل ipx | `README.md:378`، `:557-622`؛ `dgtun.sh:62-70` |
| `min_links`، `max_links` (`0`=خودکار، عدد=ثابت، حذف‌شده=32)، `per_link`، `drain_idle_sec` | پاکت pool | `README.md:29`، `:44`، `:111-120`، `:163-167`؛ `BUILD.md:74-79` |
| `forward_ports`، `udp`، `user_listen_ip`، `bind_local_ip` | درگاه‌های کاربر و نشانی‌ها | `README.md:677-678`، `:829` |
| `expose`، `port_map` | پنل پیش‌فرض و مقصد هر درگاه (روی خارج) | `README.md:334-376` |
| `backend_addr`، `cover_seed` | صفحهٔ پوششی | `README.md:791-800` |
| `cert_file`، `key_file`، `sni`، `shared_key` | TLS و احراز | `README.md:696-698`، `:776-782` |
| `tuning{mode, congestion, qdisc, rmem_max, wmem_max, netdev_backlog, somaxconn}` | تنظیم هسته | `README.md:249-261`؛ `BUILD.md:128-136`؛ کد `tune/tune.go:40-51` |
| `iface`، `local_cidr`، `peer_ip`، `mtu` | رابط TUN (`hs0`، `/30` در `10.77.0.0/16`) | `README.md:302-304` |

فرمان‌های مدیریتی مستندشده: `hs2 status [--watch]`، `hs2 doctor`، `hs2 check`، `hs2 tune [--apply]`، `hs2 recommend-links [--why] [-c]`، `hs2 config get|set|unset`، `hs2 ports …`، `hs2 version`، `hs2 cleanup` (`README.md:98`، `:160-167`، `:642`، `:802-823`؛ کد `cmd/hs2/main.go:220-250`). **مشاهده**: `BUILD.md:59-66` از `doctor`، `ports`، `recommend-links` و `cleanup` نامی نمی‌برد.

### 9.2 متغیرهای محیطی
| متغیر | اثر | مستند | کد |
|---|---|---|---|
| `GOMEMLIMIT` | بازنویسی حد حافظهٔ نرم | `README.md:136` | `cmd/hs2/main.go:310` |
| `HS2_NO_TUNE=1` | sysctlها اعمال نمی‌شوند (آزمایشگاه) | `BUILD.md:135-136` | `cmd/hs2/main.go:352` |
| `HS2_TUNE_NOTSENT`، `HS2_TUNE_SMUX_FRAME`، `HS2_TUNE_SMUX_STREAMBUF`، `HS2_TUNE_SMUX_SESSBUF`، `HS2_TUNE_CC` | بازنویسی تنظیم مسیر داده؛ **فقط حامل‌های جریانی** | `BUILD.md:202-204`؛ `run.sh:16` | `cmd/hs2/main.go:262-274` |
| `HS2_DG_FQ=0` | خاموش‌کردن صف منصفانهٔ datagram | `README.md:479-480`؛ `VALIDATION.md:166` | `engine/dgfq.go:67` |
| `HS2_FAIR_SHARE=0` | خاموش‌کردن چهار قاعدهٔ سهم گلوگاه | `README.md:509` | `udpcarrier/rate.go:309` |
| `HS2_TUN_OFFLOAD=0` | خاموش‌کردن offload در TUN | `README.md:527` | `cmd/hs2/main.go:840` |
| `HS2_RAW_BATCH=0` | یک datagram در هر فراخوان سیستمی (سوکت فقط‌ارسال را هم خاموش می‌کند) | `README.md:527-528` | `encap/raw_linux.go:400`؛ `udpcarrier/batch_linux.go:19` |
| `HS2_RAW_TX=0` | سوکت فقط‌ارسال icmp و ارسال بدون قفل در udp خاموش | `README.md:541` | `encap/rawtx_linux.go:68`؛ `udpcarrier/batch_linux.go:26` |
| `HS2_DG_PAD=0` | بدون لایه‌گذاری اندازه | `README.md:581-582` | `udpcarrier/carrier.go:20` |
| `HS2_ICMP_CAMO=1` | استتار زمان‌بندی و شناسهٔ icmp (روی هر دو سر) | `README.md:584-602`؛ `VALIDATION.md:260-272` | `udpcarrier/carrier.go:35`؛ `encap/raw_linux.go:29` |
| `HS2_ICMP_SUPPRESS=nft|iptables|global` | روش حذف پاسخ ping هسته | `README.md:640-647` | `encap/echoguard_linux.go:31-32` |
| `HS2_PPROF=127.0.0.1:port` | پروفایل‌گیری فقط روی loopback | `README.md:528-530`؛ `VALIDATION.md:205-207` | `cmd/hs2/pprof.go:12-41` |
| `HS2_REPO_RAW`، `HS2_KEEP_BACKUPS` | سنجاق‌کردن ثبت در نصب‌کننده؛ شمار نسخه‌های پشتیبان | `README.md:651-654`؛ `VALIDATION.md:21` | `install.sh:3907` |
| `HS2_TUN_REORDER_MS`، `HS2_TUN_RCVBUF` | نگه‌داشت مرتب‌سازی دوباره در dgtun؛ بافر دریافت TUN | **در README، BUILD و VALIDATION نیامده‌اند** | `engine/reorder.go:47-50`؛ `engine/dgforward.go:75-77` |
| `HS2_VERIFY_SECS` | مدت انتظار نصب‌کننده برای «آماده» (پیش‌فرض 60) | مستند نشده | `install.sh:902` |
| `HS2_DGTRACE`، `HS2_FEC_SWEEP` | ردگیری `dglab`؛ جاروب FEC در آزمون‌ها | فقط در `udpcarrier/REPORT.md:156` و `fec/README.md:46` | `dglab/main.go:295` |

متغیرهای هر اسکریپت آزمایشگاه در سرخط خودشان آمده‌اند: `run.sh:8-16`، `dgtun.sh:12-16`، `encap.sh:14-18`، `cpuquota.sh:28-39`، `transport-probe.sh:15-18`. **مشاهده**: `SEG` و `UDP` در `dgtun.sh:26`، `:111` به کار می‌روند ولی در سرخط فهرست نشده‌اند.

---

## 10. آزمون‌ها: چه چیزی تضمین می‌شود

### 10.1 آزمون‌هایی که مستقیم از ابزار آزمایشگاه استفاده می‌کنند
- **`udpcarrier/lab_test.go`** (با `lab/netsim`؛ در `-short` رد می‌شوند):
  - `TestLabBursty26` (`:204-256`): مسیر 20 Mbit/s، 25 ms، و GE با 88٪ اتلاف در انفجارهای 12 ms و 2٪ در بقیهٔ زمان. شرط‌ها:
    - اتلاف سیم دست‌کم 12٪؛
    - اتلاف باقی‌مانده حداکثر 0.4 برابر اتلاف سیم و حداکثر 14٪؛
    - goodput دست‌کم 6.5 Mbit/s، یعنی گلوگاه پر نگه داشته می‌شود؛
    - jitter (p95 منهای p50) حداکثر 320 ms.
  - `TestLabAdaptiveStep` (`:260-288`): اتلاف وسط آزمون از حدود 5٪ به حدود 50٪ می‌پرد. شرط‌ها:
    - parity در پایان دست‌کم 0.3؛
    - اتلاف باقی‌مانده حداکثر 0.6 برابر اتلاف سیم و حداکثر 20٪.
  - `TestLabOverheadLowVsHigh` (`:292-318`): سربار FEC با بالارفتن اتلاف زیاد می‌شود.
- **`engine/carrier_udp_test.go`:**
  - `TestAutoSelectsUDPWhenGood`؛
  - `TestProbeSeesBlockedUDP` (با `netsim.AllDrop`)؛
  - `TestAutoFallsBackToTCP`: بازگشت auto به حامل **noise روی TCP**، که با `README.md:756-758` نمی‌خواند (بخش 13).

### 10.2 آزمون‌هایی که مستندات نام می‌برند (وجودشان بررسی شد)
- `BUILD.md:122-126`: `engine/autopilot_sim_test.go`، `engine/autopilot_test.go`، `engine/pool_v2_test.go`، `engine/stats_test.go`، `engine/stream_reverse_test.go`، `engine/stream_v2_test.go`. همه موجودند. آزمون‌های دیگر در همین حوزه: `autopilot_scale_test.go`، `regress300_test.go`، `refill_test.go`، `stuck_test.go`، `loss_test.go`، `wedge_test.go`، `l3_link_test.go`، `dgmute_test.go`، `dgfq_test.go`.
- `BUILD.md:173`: `TestMITMRejected` در `tlscarrier/carrier_test.go:222`، و هم‌نام آن در `udpcarrier/security_test.go:20`.
- `BUILD.md:135`: `tune/tune_test.go`.
- `CHANGELOG.md:1797-1809`: `go test ./...` و `-race`، آزمون‌های bash و pty، `install/tests/release_files_test.sh`، `shellcheck`.
- `release_files_test.sh` تضمین می‌کند:
  - `install.sh` ریشه با `hs2-src/install/install.sh` بایت‌به‌بایت یکی است (اکنون یکی است؛ با `cmp` بررسی شد)؛
  - `hs2-linux-amd64.sha256` با باینری می‌خواند (بررسی شد)؛
  - `install.sh.sha256` با `install.sh` می‌خواند.
- `install/e2e/README.md`: حدود 70 رفتار نصب‌کننده در دو کانتینر Ubuntu با systemd.

### 10.3 آنچه آزمونی ندارد
- **هیچ آزمونی مستندات را با کد مقایسه نمی‌کند.** تنها ارجاع آزمون‌ها به مستندات توضیح‌های `udpcarrier/rate_pool_sim_test.go:676`، `:720` است.
- **خود اسکریپت‌های آزمایشگاه** (`run.sh`، `dgtun.sh`، …) **آزمون خودکار ندارند**؛ برای نمونه، کهنه‌شدن الگوی `link_churn` در `run.sh` (بخش 13) دیده نشد.
- **ابزار آزمون بار سنگین فازهای Q6 تا Q8** (`run3.py`، «rig» با سه فضای نام و 5,500 کاربر) **در مخزن نیست** (`CHANGELOG.md:984` نامش را می‌برد). در `hs2-source.zip` هم نیست.

---

## 11. «از قبل وجود دارد» (فهرست صریح برای جلوگیری از دوباره‌کاری)

**الگوی پیوندها و هستهٔ جریانی (mtcp و l3mtcp):**
1. autopilot با کف `ceil(active/per_link)`، رشد آزمایشی (probe) با تأیید افزایشی بودن، عقب‌نشینی نمایی تا 8 min، حافظهٔ «مسیر پر»، کوچک‌شدن بر پایهٔ تقاضا و برگرداندن آن (`BUILD.md:71-102`).
2. سقف خودکار بر پایهٔ RAM و هسته (تا 300)، با توجه به cgroup؛ تبادل سقف دو سرور (`kindInfo`)؛ نمایش سقف مؤثر روی هر دو سرور (`README.md:109-167`).
3. آغاز گرم با 8 پیوند، فایل `.warm`، دروازهٔ شماره‌گیری سراسری، کند بالا آمدن در قطعی سمت خارج، نگه‌داشت refill (`README.md:183-217`).
4. قاعدهٔ اتلاف دوجهته با پنجرهٔ pong، مخرج کسر از TCP_INFO، تشخیص گلوگاه هر اتصال، تشخیص «مسیر پراتلاف»، سقف تخلیهٔ یک‌هشتم (`README.md:46-68`).
5. قاعدهٔ گیرکردن بر پایهٔ عمر پینگ کنترلی، تشخیص «مسیر کند» و «مسیر پرازدحام» با کف RTT ده‌دقیقه‌ای، پنجرهٔ بازیابی (`README.md:69-90`).
6. حالت مشکوک پس از 12 s؛ تخلیهٔ مرحله‌ای پیوند degraded (45 s و 90 s).
7. نگهبان خوانندهٔ گیرکرده، و حالت فشار حافظهٔ TCP هسته که بین دو سرور اعلام می‌شود (`README.md:218-233`).
8. صف و نویسندهٔ جدا برای هر جریان UDP (`README.md:244-247`).
9. کانال جانبی l3 با سقف ماندگاری 60 ms، hash rendezvous، keepalive کم‌صدا، جابه‌جایی در 12 s (`README.md:234-235`؛ `engine/l3_link.go`).
10. BBR، `TCP_NOTSENT_LOWAT=32KiB`، `TCP_USER_TIMEOUT=20s`، قاب 16 KiB، keepalive تصادفی 4 تا 8 s، MPTCP خاموش (`CHANGELOG.md:827-862`).
11. پخش اتصال‌های هم‌زمان روی پیوندها (`README.md:667-669`).
12. احراز دوطرفهٔ وابسته به TLS exporter، صفحهٔ پوششی متفاوت برای هر نصب، پاسخ همسان با سرور HTTPS واقعی (`README.md:776-800`).

**تونل datagram (dgtun):**

13. تشخیص پلیس‌گر کل pool، سقف، تأیید، برداشتن سقف، parity بر پایهٔ اتلاف خود مسیر.
14. صف منصفانه با خط تند برای جریان‌های تعاملی.
15. چهار قاعدهٔ سهم گلوگاه (خروج از شروع سریع، رشد به سمت سهم، ساعت مشترک کاوش پایه، قاعدهٔ مرحلهٔ ارسال).
16. `sendmmsg` و `recvmmsg`؛ offload در TUN؛ سوکت فقط‌ارسال icmp.
17. تشخیص اشباع پردازنده و ذخیرهٔ بیشتر pacer هنگام اشباع؛ حامل بی‌صدا در 1 s.
18. سقف 8 برای icmp؛ پوشاندن سرآیند icmp؛ استتار اختیاری.

**ابزار:**

19. `hs2 status`، doctor، check، `recommend-links`، `cleanup`؛ فایل وضعیت زنده.
20. ابزار آزمایشگاه: netem با پلیس‌گر جریان و مقصد، GE، jitter و فهرست پروتکل مجاز؛ netsim؛ probe؛ dglab؛ cpuquota با QUOTA و SHARE؛ transport-probe برای مسیر واقعی؛ sweep و analyze.

---

## 12. ایده‌هایی که امتحان و رد شده‌اند (طبق کد و مستندات)

| ایده | چرا رد شد | منبع |
|---|---|---|
| حمل بستهٔ IP روی TLS در `tls` و `l3mtcp` (TCP درون TCP) | صف‌های چندثانیه‌ای زیر بار؛ جایش را هستهٔ جریانی گرفت | `README.md:659-666` |
| Noise درون TLS | AEAD دوم هزینهٔ CPU دارد و امنیتی اضافه نمی‌کند | `BUILD.md:179-180` |
| `TCP_NOTSENT_LOWAT` با 64 KiB یا بیشتر، یا خاموش | تأخیر بیشتر روی پیوند کند؛ خاموش‌بودن 3 تا 7 برابر بدتر | `tlscarrier/tune_linux.go:12-19` |
| autopilot نسخهٔ اول | تا 32 بالا می‌رفت و پایین نمی‌آمد؛ امروز هیچ ورودی قفل نمی‌شود | `engine/autopilot.go:39-44` |
| تعداد ثابت 4 برای «پیوند آزاد» | در poolهای صدتایی رشد را زودتر از نیاز می‌بست؛ اکنون بالای 64 یک‌شانزدهم | `engine/autopilot.go:279-298` |
| بستن همهٔ کاربران پیوند degraded پس از 45 s | در آزمون بار 60 تا 90 اتصال فعال در هر پیوند قطع می‌شد | `engine/health.go:72-73`؛ `CHANGELOG.md:832-840` |
| نگه‌داشتن کاربران پیوند پراتلاف تا 5 دقیقه | p90 بین 1.6 و 2.7 s در تمام آن مدت | `CHANGELOG.md:842-843`؛ `engine/health.go:66-71` |
| آهنگ jitter‌دار برای پینگ کنترلی | هشدار نادرست اتلاف دانلود را برگرداند | `CHANGELOG.md:769-770` |
| سنجش اتلاف در هر تیک 2 ثانیه‌ای با زمان pong | زمان‌بندی pong را می‌خواند نه اتلاف را | `CHANGELOG.md:991-999`؛ `engine/health.go:140-149` |
| مخرج اتلاف از تقسیم بایت‌ها بر 1400 | 1.1 تا 10 برابر اغراق | `CHANGELOG.md:1000-1004`؛ `engine/health.go:204-210` |
| مقایسهٔ پیوند با میانهٔ پیوندهای مشغول | پیوندهای پراتلاف را لابه‌لای پیوندهای درگیر گلوگاه پنهان می‌کرد | `engine/loss.go:48-51` |
| نسخه‌های قبلی قاعدهٔ اتلاف | در ازدحام به‌ترتیب 210، 55 و 27 پیوند را تخلیه کردند | `CHANGELOG.md:964-966` |
| شمردن پیوندهای با پاسخ 2 تا 6 s، یا بی‌پاسخ، به‌عنوان «کند» | دو پیوند پراتلاف در شب هر دو قاعده را بی‌صدا خاموش می‌کردند | `CHANGELOG.md:894-897` |
| سقف refill که هر 2 s دوبرابر شود | پیوندهای اول دوباره پر می‌شدند (192 در برابر 96) | `engine/refill.go:30-34` |
| شنونده‌های MPTCP (پیش‌فرض Go 1.24 به بعد) | `tcp_notsent_lowat` را نادیده می‌گرفتند؛ p99 به 8 s رسید | `CHANGELOG.md:827-833` |
| یک سوکت خام برای هر حامل | در 300 حامل GRE ‏28 در برابر 176 Mbit/s | `README.md:122-127`؛ `CHANGELOG.md:734-736` |
| صف FIFO برای هر حامل datagram | ping زیر بار 66/94 ms؛ با صف منصفانه 12/20 ms | `CHANGELOG.md:1162-1195` |
| سکوت کامل حامل بی‌کار در استتار (CA1) | با آشکارساز mute (حدود 1 s) قطع و وصل می‌شد؛ CA1b ضربان حدود 0.7 s گذاشت | ثبت `da621f5`؛ `udpcarrier/carrier.go:22-45` |
| CA3: نگه‌داشتن بسته‌ها زیر آستانهٔ اندازه | توان 6 تا 7 برابر کمتر و سیلی از بسته‌های کوچک (نشانهٔ تازه)؛ برای حالت اضطراری کنار گذاشته شد | `CHANGELOG.md:1501-1505` |
| CA4: شمارندهٔ سادهٔ echo یا جفت‌کردن درخواست و پاسخ | هر گونه‌اش نشانهٔ بدتری می‌ساخت؛ فیلتر میدانی به اندازه و حجم نگاه می‌کند نه این همبستگی | `CHANGELOG.md:1505-1512`؛ `README.md:597-602` |
| صف ارسال عمیق‌تر هنگام اشباع (Phase Y) | عدد را تکان نداد | `CHANGELOG.md:1553-1554` |
| نرخ بی‌سقف مثل حالت قواعد خاموش (Phase Y) | با آزادشدن CPU بافر مسیر مشترک را سیل می‌کند؛ فاصلهٔ حدود 14٪ عمداً مانده | `CHANGELOG.md:1554-1560`؛ `VALIDATION.md:221-225` |
| نگاه 5 بایتی پیش از TLS به همراه `prefixConn` | RST به‌جای FIN؛ TCP_INFO سمت سرور خاموش می‌شد | `CHANGELOG.md:162-171`، `:218-230` |
| صفحهٔ پوششی ثابت | یک هش مشترک برای همهٔ سرورها؛ جایش را `cover_seed` گرفت | `CHANGELOG.md:187-191`؛ `README.md:791-800` |
| سقف میانی dgtun (64 روی raw و 128 روی udp) | پس از آزمون 300 حامل برداشته شد | `README.md:122-127` |

---

## 13. محدودیت‌های شناخته‌شده و مشاهده‌ها

### 13.1 محدودیت‌هایی که خود مستندات اعلام کرده‌اند
- **`tls` تک‌پیوند** نمی‌تواند از سقف هر اتصال بالاتر برود (`README.md:690-691`).
- **روی مسیر کندی که سقف هر اتصال ندارد**، پیوند کمتر یعنی تأخیر کمتر زیر بار پر: روی مسیر 8 Mbit ‏333 ms در برابر 628 ms (`hs2-src/README.md:114-118`؛ `README.md:769-774`).
- **آنچه نگهبان نمی‌بیند** (`README.md:242-247`):
  - تا چهار جریان منتظر شماره‌گیری کند پنل می‌توانند پیوند را تا 5 s نگه دارند؛
  - hs2 قدیمی‌تر روی سرور دیگر بافرهای بی‌نگهبان دارد.
- **گیرکردن سمت خارج** (خوانندهٔ پارک‌شده پشت مقصد کند) از لبه دیده نمی‌شود (`CHANGELOG.md:977-981`؛ `engine/stuck.go:55-60`).
- **صدها پیوند TLS** میان یک جفت IP غیرعادی به نظر می‌رسد؛ این تصمیم با مالک تونل است (`README.md:169-181`).
- **icmp حجم را پنهان نمی‌کند** (`README.md:609-622`). استتار CA همبستگی دنباله و درخواست/پاسخ را عوض نمی‌کند (`README.md:596-602`).
- **رفتارهایی که rig نشان داد و تغییر نکردند** (`CHANGELOG.md:1085-1097`):
  - هجوم ناگهانی به pool کوچکی که از قبل بالاست، پیوندهای اول را شلوغ نگه می‌دارد؛ refill فقط شروع یا قطعی کامل را پوشش می‌دهد؛
  - 10,000 اتصال باز حدود 0.9 GB RSS دارند؛
  - **انفجار اتصال‌های تازه (بیش از حدود 40 در دقیقه برای هر پیوند)** فشار واقعی پیوند را از چشم جایگذاری پنهان می‌کند، پس 6 تا 10 درصد اتصال‌های تازه روی پیوندهای کندشده می‌نشینند؛
  - probe لبهٔ reverse از سقف خروجی خبر ندارد.
- **V5 باز است** (`VALIDATION.md:111-119`): در ازدحام با صف کم‌عمق (20 ms)، اتصال‌های قطع‌شده بیشتر از main بود.
- **datagram** (`CHANGELOG.md:1424-1466`):
  - پلیس‌گر پایدار بدون رویداد اتلاف باعث ارسال حدود 2.5 برابر می‌شود؛
  - 16 حامل روی 8 Mbit فرو می‌ریزند؛
  - 8 حامل روی 16 تا 24 Mbit با RTT ‏80 ms و Jain بین 0.13 و 0.26؛
  - جریانی بیرون از pool که صف را بالای 100 ms نگه دارد همهٔ حامل‌ها را می‌فشارد.
- **UDP/auto** (`udpcarrier/REPORT.md:115-145`):
  - درگاه‌گردان نیستند (فقط IP روی hs0)؛
  - بازگشت به TCP آنی نیست (حدود 15 s)؛
  - کنترل نرخ «BBR سبک» است.

### 13.2 ناسازگاری‌های مستند و کد (هر کدام با کد بررسی شد)
1. **ناسازگار — README:756-758 دربارهٔ `auto`:**
   - مستند: auto «اول udp را می‌کاود، بعد به tcp/TLS برمی‌گردد؛ این‌ها حامل‌های TLS هستند که ممکن است به آن برسد: mtcp، l3mtcp، tls».
   - کد: بازگشت auto به حامل **noise روی TCP** در موتور بسته‌ای است (`engine/carrier_udp.go:69-90`؛ آزمون `TestAutoFallsBackToTCP`)، نه mtcp، l3mtcp یا tls. auto درگاه کاربر هم ندارد (`install.sh:2190`؛ `REPORT.md:131-134`).
2. **ناسازگار — README:389، بازکاوش پلیس‌گر:** مستند «5٪ هر 10 s» می‌گوید؛ کد 15٪ هر 4 s است، به شرط پاک بودن و گلوگاه بودن سقف (`udpcarrier/governor.go:125-126`، `:470-471`).
3. **ناسازگار — README:588-590، استتار icmp:** مستند می‌گوید حامل بی‌کار «ساکت می‌شود و فقط keepalive حدود 5 s دارد». کد پس از CA1b ضربان حدود 0.7 s با jitter دارد و عمداً کاملاً ساکت نمی‌شود (`udpcarrier/carrier.go:26-30`، `:37-41`). `CHANGELOG.md:1477-1483` هم هنوز نسخهٔ CA1 را شرح می‌دهد؛ فقط سطر انتشار در `VALIDATION.md:279-282` از CA1b نام می‌برد.
4. **ناسازگار — README:839-840 و hs2-src/README.md:151-152:** می‌گویند «نصب‌کننده تنظیم BBR/fq را سراسری در `/etc/sysctl.d/99-hs2.conf` می‌گذارد». نصب‌کنندهٔ امروز این فایل را **پاک** می‌کند (`install.sh:4088-4090`) و تنظیم در هر شروع از خود hs2 می‌آید (`README.md:249-261`؛ `BUILD.md:132-133`؛ `install/e2e/README.md`: «no static sysctl file»). صف پیش‌فرض هم `fq_codel` است نه `fq` (`tune/tune.go:300`).
5. **ناسازگار جزئی — README:18-19 (و hs2-src/README.md:18-19):** «پیوند مرده حدود دو ثانیه‌ای تشخیص داده می‌شود».
   - این فقط برای پیوندی درست است که صریحاً بسته شود (RST یا FIN؛ `Alive()` در تیک 2 s).
   - پیوند سیاه‌چاله‌شده:
     - 12 s بعد suspect می‌شود (کاربر تازه نمی‌گیرد)؛
     - با `TCP_USER_TIMEOUT=20s` (فقط اگر داده‌ای بی‌تأیید مانده باشد) یا `KeepAliveTimeout=24s` بسته می‌شود.
   - مرجع‌ها: `engine/linkmanager.go:310-317`؛ `tlscarrier/tune_linux.go:22`؛ `engine/mtcp_link.go:279`.
6. **ناسازگار جزئی — README:627:** «تا 40 s صبر می‌کند». پیش‌فرض کد 60 s است (`HS2_VERIFY_SECS`، `install.sh:902`، `:4173`، `:978`).
7. **ناسازگاری درونی README — README:471 در برابر README:239-241 و 543-549:**
   - :471: «هر جریان درونی تا پایان عمرش به یک حامل سنجاق است؛ تغییر اندازهٔ pool جریان زنده را جابه‌جا نمی‌کند».
   - :239-241 و :543-549: جریان‌های حامل در حال بازنشستگی پس از حداکثر 30 s جابه‌جا می‌شوند (`dgRetireForce`، `engine/dgpool.go:347`) و جریان‌های حامل بی‌صدا هم.
8. **کهنه — hs2-src/README.md (کل سند):**
   - «8–16 links» (`:32`، `:110-111`) و «mtcp: default» (`:110`)، در حالی که امروز pool تطبیقی از 2 تا سقف سخت‌افزاری است و نصب‌کننده auto را توصیه می‌کند؛
   - منوی `bash install.sh # 3 = uninstall …` (`:133`) با `hs2-menu` امروز (`README.md:805`) فرق دارد؛
   - پیش‌نیازها می‌گویند دامنه همیشه لازم است (`:55`)، اما برای udp و auto و dgtun لازم نیست (`README.md:699-700`).
9. **کهنه — BUILD.md:**
   - چیدمان (`:46-57`) پوشه‌های `udpcarrier/`، `encap/` و `mmsg/` را ندارد؛
   - فهرست فرمان‌ها (`:59-66`) `doctor`، `ports`، `recommend-links` و `cleanup` را ندارد؛
   - بخش آزمایشگاه (`:182-204`) از `dgtun.sh`، `encap.sh`، `cpuquota.sh`، `transport-probe.sh`، `dglab` و `netsim` نامی نمی‌برد، و توان‌های netem (jitter، GE، پلیس‌گر مقصد، `allow`، `dropudp`) را هم نمی‌گوید؛
   - دستور هش (`:19-24`) بازسازی `install.sh.sha256` را نمی‌گوید، در حالی که `release_files_test.sh` آن را بررسی می‌کند. مسیرهای دستور `cmp install.sh install/install.sh` هم با چیدمان مخزن (فایل ریشه در برابر `hs2-src/install/`) نمی‌خواند.
10. **مستند نشده:** `retireForce=20m` (بستن اتصال‌های کم‌حرکت روی پیوند بازنشسته پس از 20 min)، `refillRearm=1m`، `holdMax=2h`، `probeStepMax`، `HS2_TUN_REORDER_MS`، `HS2_TUN_RCVBUF`، `HS2_VERIFY_SECS`.
11. **دقت عبارت «یک‌هشتم pool»:** `drainHeadroom(n) = max(2, ceil(n/8))` (`engine/linkmanager.go:1253`)؛ در poolهای کوچک (کمتر از 16) تا 2 پیوند هم‌زمان تخلیه می‌شوند.
12. **نکتهٔ دامنهٔ ادعا — README:10 «هرگز TCP درون TCP نیست»:** فقط برای درگاه‌های کاربر درست است. در l3mtcp و tls، هر TCP که کاربر خودش روی `hs0` مسیریابی کند **درون TCP پیوند** می‌رود (کانال جانبی، `engine/l3_link.go:22-29`)؛ مستند هم آن را «نه مسیر حجیم» می‌نامد (`README.md:659-666`).
13. **README:305 و :696** «tcp» و «tun-over-tcp» را به معنای اصطلاح نصب‌کننده به کار می‌برند (در `install.sh:2657`، tcp یعنی mtcp و «tun over TLS» یعنی l3mtcp یا tls)، نه حامل `noise`. خواننده‌ای که از کد بیاید ممکن است گیج شود.
14. **عنوان VALIDATION.md** (`:1`) فقط Q7، Q8، V، W و X را نام می‌برد، اما سند CA (V14) و Y (در V13) را هم دارد. ترتیب بخش‌ها هم V11، V13، V12 است.
15. **سرخط `dgtun.sh:97-99`:** می‌گوید قاعدهٔ nft/iptables «بر پایهٔ magic تونل» است؛ کد امروز آن را روی بایت بالای دنبالهٔ echo می‌گذارد (پیشوند کلیددار c2s، `encap/echoguard_linux.go:25-32`) و magic از سیم icmp حذف شده (`encap/obfs.go:24-29`). اصطلاح کهنه است، رفتار درست است.

### 13.3 مشاهده‌ها دربارهٔ آزمایشگاه و روش اعتبارسنجی (بدون پیشنهاد تغییر)
- **مشاهده — `link_churn` در `run.sh` در عمل همیشه صفر است:**
  - الگو `reap:|rebuilt|link down` است (`run.sh:90`)، اما لاگ امروز لبه `mtcp: link %d down: …` است (`engine/linkmanager.go:1429`)؛ میان «link» و «down» یک عدد هست.
  - خلاصهٔ انفجاری `+K more links down` هم با «link down» نمی‌خواند (`engine/burstlog.go:71`).
  - رشته‌های `reap:` و `rebuilt` در لاگ‌ها وجود ندارند.
  - پس ستون churn در نتایج تازهٔ جاروب معنا ندارد.
- **مشاهده — `run.sh` فقط direct را پوشش می‌دهد** (نه `REVERSE`)، و فقط `-rate -delay -queue -loss -flowrate` از netem را. اتلاف انفجاری، jitter و پلیس‌گر مقصد برای حامل‌های جریانی در دسترس نیست.
  - طبق VALIDATION و CHANGELOG، تولید و آزمون‌های بار عمدتاً reverse هستند (`VALIDATION.md:64-66`؛ `CHANGELOG.md:912`، `:1036`).
  - دستور بازتولید `udpcarrier/REPORT.md:159-160` (`MODE=… BURSTLOSS=… BADMS=…`) به «scratch scripts» تکیه دارد که در مخزن نیستند.
- **مشاهده — `run.sh` برای `MODE=udp|auto|noise|reality` کار نمی‌کند:**
  - udp و auto درگاه‌گردان ندارند (`cmd/hs2/main.go:795-821`)، پس probe به 8443 وصل نمی‌شود؛
  - noise و reality کلید یا `cover_addr` می‌خواهند که پیکربندی آزمایشگاه نمی‌دهد. **نامطمئن** که به چه خطایی می‌انجامد؛ اجرا نشد.
- **مشاهده — جدول v2→v3 در README:680-691** همان جدول `hs2-src/README.md:39-50` است. آن نسخه کنار «8–16 links» آمده (`hs2-src/README.md:30-32`). **احتمالاً (نامطمئن)** با pool ثابت 8 تا 16 و پیش از autopilot (v3.2) اندازه‌گیری شده و پس از autopilot دوباره سنجیده نشده است. داده‌های خام (`results.jsonl`) در مخزن نیست.
- **مشاهده — `probe` پس از نخستین echo گم‌شده** (مهلت 5 s) حلقهٔ echo را تمام می‌کند (`probe/main.go:244-249`). پس `echo_lost` حداکثر 1 است و پس از آن نمونه‌ای ثبت نمی‌شود. این با گزارش «interactive connection died (echo_lost, 0 samples)» در `REPORT.md:105` هم‌خوان است.
- **مشاهده — `sweep.py`** استثنای `TimeoutExpired` را نمی‌گیرد (`sweep.py:49-50`)؛ اجرای بیش از 300 s، worker را می‌کشد و بقیهٔ صف آن worker اجرا نمی‌شود.
- **مشاهده — ابزارهای کمکی کهنه می‌مانند:** `netem`، `probe` و `dglab` در `run.sh:38-39`، `dgtun.sh:35-36` و `encap.sh:38-39` فقط اگر نباشند ساخته می‌شوند. پس از تغییر کد ابزار، باینری قدیمی در `/tmp/hs2lab*` به کار می‌رود.
- **مشاهده — `transport-probe.sh`:**
  - سرخط پیش‌فرض SLOT را 30 می‌گوید (`:18`)، کد 35 است (`:22`)؛
  - فقط reverse را می‌سنجد؛
  - عدد «Mbit/s» بازگشت echo است (هم‌زمان رفت و برگشت)، نه توان یک‌طرفه (`:117-134`).
- **مشاهده — `cpuquota.sh`** فرض می‌کند `CLK_TCK=100` (`:169`).
- **مشاهده — `hs2-source.zip`** (ریشه) تصویر 2026-09-28 است (حدود 80 فایل Go؛ `lab/netem/main.go` با 7600 بایت در برابر 13443 بایت امروز). با درخت `hs2-src` هم‌گام نیست.
- **مشاهده — l3mtcp:**
  - کانال جانبی جریان‌ها را با hash rendezvous فقط بر پایهٔ `Alive()` همان جریان l3 پخش می‌کند (`engine/l3_link.go:308-323`)، نه بر پایهٔ وضعیت serving، retiring، degraded یا suspect پیوند در LinkManager. ترافیک hs0 تا بسته‌شدن پیوند (یا 12 s سکوت نشست) روی پیوند degraded یا retiring می‌ماند.
  - مستندات این را صریح نمی‌گویند.
  - بستهٔ پذیرفته‌شده در صف l3 پس از نوشتن در جریان smux، پشت قاب‌های حجیم همان پیوند می‌ایستد؛ سقف 60 ms فقط زمان صف خود l3 را می‌سنجد (`engine/l3_link.go:206-213`).
- **مشاهده — آزمایشگاه بار سنگین (rig) و ابزار فازهای Q6 تا Q8 در مخزن نیستند.** عددهای کلیدی README دربارهٔ 300 پیوند، 5,500 کاربر، راه‌اندازی دوباره و قطعی فقط در `CHANGELOG.md` ثبت‌اند و از مخزن بازتولیدپذیر نیستند.
- **مشاهده — راستی‌آزمایی میدانی CA** طبق خود VALIDATION (`:283-286`) فقط روی یک جفت آزمایشی دور از مرز انجام شده است. اثبات روی‌سیم V14 و رفع قطع‌ووصل هنوز اجرای جداگانه می‌خواهد.

### 13.4 نتایج ثبت‌شده (برای مرجع سریع)
| آزمایش | شرایط | نتیجه | منبع |
|---|---|---|---|
| v2→v3 | میانهٔ 3 اجرا، RTT ‏80 تا 120 ms؛ «throttled» = اتلاف 0.5٪ و 10 Mbit/s برای هر اتصال | mtcp: ‏37.3→47.2 Mbit/s، ‏234→202 ms، ‏340→329 ms؛ l3mtcp: ‏34.8→47.1، ‏256→203، ‏526→343؛ tls روی مسیر تمیز 50: ‏42.2→46.3، ‏201→114، ‏399→138؛ mtcp روی مسیر کند 8 Mbit: ‏7.5→7.6، ‏1078→628، ‏1576→394 | `README.md:680-691` |
| یک پیوند در برابر 8 پیوند | مسیر 8 Mbit | تأخیر زیر بار 333 در برابر 628 ms | `hs2-src/README.md:114-117` |
| سقف (فاز H) | TLS واقعی روی loopback، 400 اتصال | جدول سقف مؤثر direct و reverse؛ RSS ‏55 تا 67 MiB با 32 تا 50 پیوند | `CHANGELOG.md:508-525` |
| آزمون بار Q6 | 400 Mbit/s، ‏40±5 ms، پلیس‌گر 3 Mbit/s برای هر جریان، 5,416 اتصال | 300 پیوند در حدود 57 s؛ حدود 48٪ یک هسته؛ راه‌اندازی دوبارهٔ ایران: ‏16.3 s (قبلاً 71 s)؛ قطعی 40 s: ‏16 تا 20 s (قبلاً 61 s) | `CHANGELOG.md:805-821` |
| Q7 پیوند گیرکرده | حدود 200 پیوند، 1,600 کاربر فعال، 3 بسته در ثانیه | 9 از 10 در 10.6 s؛ پاسخ echo از 76٪ به 100٪ | `CHANGELOG.md:912-950` |
| Q8 پهنای باند بالا | 300 پیوند، 150 دانلود، 150 تا 200 Mbit/s | حکم اتلاف در حالت پایدار 28→1؛ ازدحام: 1,738→99 قطع | `CHANGELOG.md:1036-1058` |
| dgtun با 300 حامل | gre و udp | gre: ‏176 در برابر 28 Mbit/s؛ udp: ‏p50 90 ms و p99 121 ms | `README.md:122-127` |
| صف منصفانه | گلوگاه 20 Mbit/s، 8 دانلود | ping p50 ‏66→12 ms با همان توان | `README.md:478-479`؛ `CHANGELOG.md:1184-1195` |
| سهم گلوگاه | 30 Mbit/s، 4 حامل، 8 دانلود | ping p50 ‏17 تا 19 ms، p99 حداکثر 34 ms (قبلاً 11 تا 114 ms) | `README.md:491-495` |
| offload | icmp، reverse، 2 هسته | حدود 450 به حدود 650 Mbit/s؛ CPU بر گیگابایت 27 تا 39 درصد کمتر | `README.md:525-527` |
| فرستندهٔ کم‌پردازنده | 0.6 هسته | 212→270 Mbit/s | `README.md:506-508` |
| Phase Y | SHARE، حدود 0.4 هسته | فاصله با حالت قواعد خاموش از حدود 27٪ به حدود 14٪ | `CHANGELOG.md:1549-1560` |
| تعداد حامل‌ها | فرستندهٔ 2 هسته‌ای؛ 1، 3 و 8 حامل | 903، 1393 و 1453 Mbit/s | `VALIDATION.md:188-189` |
| UDP در برابر TCP | 20 Mbit/s، اتلاف انفجاری حدود 26٪ | udp ‏1.9 Mbit/s بدون echo گم‌شده؛ mtcp ‏2.9 Mbit/s اما اتصال تعاملی مُرد | `udpcarrier/REPORT.md:94-113` |
| FEC | 26٪ i.i.d. | باقی‌مانده 0.12٪، سربار 69٪ | `fec/README.md:43-64` |

---

## 14. ارجاع به زیرسیستم‌های دیگر

- **`cmd/hs2`** (نقشه‌های `01-cmd-runtime.md` و `02-cmd-ops-tune.md`):
  - `applyTuning` و متغیرهای `HS2_TUNE_*` (`main.go:258-274`)؛ `HS2_NO_TUNE` (`:352`)؛
  - `linkCeiling` و `icmpMaxLinks` (`:678-724`)؛ `status`، `doctor`، `check`، `recommend-links`؛
  - فایل‌های `/run/hs2/*.status.json` و `.warm` که اسکریپت‌های آزمایشگاه (`cpuquota.sh` با `hs2 status`) و VALIDATION می‌خوانند.
- **هستهٔ جریانی و LinkManager و autopilot و سلامت** (نقشه‌های `03-stream-core.md`، `04-linkmanager.md`، `05-autopilot-health.md`): تقریباً همهٔ ادعاهای README:21-247 و BUILD:71-126 و سناریوهای V1 تا V8 دربارهٔ این‌هاست. `run.sh` و `sweep.py` همین مسیر را می‌سنجند.
- **کانال جانبی l3 و TUN** (نقشهٔ `06-l3-tun.md`): `engine/l3_link.go` و `engine/stream_iran.go:418-438`؛ ping روی hs0 در `run.sh:80-84`.
- **TLS، obfs و core** (نقشهٔ `07-tls-obfs-core.md`): `tlscarrier/tune_linux.go` (BBR، NOTSENT، USER_TIMEOUT)، احراز (`TestMITMRejected`)، صفحهٔ پوششی.
- **datagram** (`engine/dgpool.go`، `dgfq.go`، `udpcarrier/*`، `encap/*`، `fec/*`): README:378-622، VALIDATION V9 تا V14، و `dgtun.sh`، `encap.sh`، `dglab`، `cpuquota.sh`، `netsim`.
- **نصب‌کننده** (`install.sh` و `hs2-src/install/*`): README:287-376، :624-655، :704-752؛ `release_files_test.sh`؛ `install/e2e`.
- **ابزار آزمایشگاه از کد اصلی استفاده می‌کنند:**
  - `dglab` → `udpcarrier.ListenCfg/DialCfg` و `core.TypeData`؛
  - `netsim` → در `udpcarrier/lab_test.go` و `engine/carrier_udp_test.go`؛
  - `run.sh`، `dgtun.sh`، `cpuquota.sh` و `transport-probe.sh` → باینری `hs2 run -c`.
- **هیچ کد تولیدی‌ای از `lab/` وارد نمی‌شود** جز `lab/netsim`، آن هم فقط در آزمون‌ها.
