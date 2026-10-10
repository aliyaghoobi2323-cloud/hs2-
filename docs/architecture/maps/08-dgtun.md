# dgtun: TUN روی پول حامل‌های datagram

> نقشهٔ معماری زیرسیستم `dgtun` در مخزن hs2 (ثبت `0812bc9`). همهٔ مسیرها نسبت به `/home/user/hs2-/hs2-src` هستند مگر خلافش گفته شود.
> فایل‌هایی که کامل خوانده شدند: `engine/dgpool.go` (۲۴۳۷ خط)، `engine/dgfq.go`، `engine/dgports.go`، `engine/dgforward.go`، `engine/dgcarrier.go`، `engine/carrier_udp.go`، `engine/reorder.go`، `engine/tunbatch.go`، و همهٔ آزمون‌های `engine/dg*_test.go`، `engine/carrier_udp_test.go`، `engine/stage_drop_test.go`، `engine/tunbatch_test.go` (بخش dg). برای مقایسه و زمینه: `engine/l3_link.go`، بخش‌هایی از `engine/linkmanager.go`، `engine/health.go`، `engine/autopilot.go` (تا ابتدای `decide`)، `engine/stream.go`، `engine/stream_iran.go`، `engine/peerinfo.go`، `engine/routes.go`، `engine/burstlog.go`، `engine/dialgate.go`، `udpcarrier/carrier.go` و `udpcarrier/governor.go` (بخش‌های مرتبط)، `cmd/hs2/main.go` (سیم‌کشی)، `CHANGELOG.md` و `README.md` ریشه.
> وضعیت آزمون‌ها: `go test ./engine -run 'Dg|FQ|Reorder|TunBatch|StallGate|SendDiag|CarrierEcho|AutoSelects|ProbeSees|AutoFalls|RunClosesListener' -count=1` روی همین ثبت **پاس** شد (`ok ... 24.292s`). هیچ فایلی در مخزن تغییر نکرد.

---

## ۱. نقش و جایگاه در کل سیستم

- **انتخاب حامل:** `carrier: "dgtun"` در `cmd/hs2/main.go:420-421` تابع `runDgTun` (`cmd/hs2/main.go:829-903`) را اجرا می‌کند. توضیح خود کد: «یک رابط مسیریابی‌شده (TUN) روی یک **پول** از حامل‌های datagram (udpcarrier روی encap انتخاب‌شده)، با اندازه‌گیری همان autopilot پول جریانی، به‌علاوهٔ forwarder پورت در فضای کاربر» (`cmd/hs2/main.go:823-828`).
- **هدف طراحی** (`engine/dgpool.go:18-44`): هر حامل یک لولهٔ datagram با رمزنگاری، FEC و pacing تأخیرمحور خودش است؛ هر بستهٔ IP از TUN به‌صورت **یک datagram مهروموم‌شده** عبور می‌کند. تنها TCP موجود، TCP کاربر (در واقع TCP بین دو forwarder، پایین را ببینید) است که به‌صورت بستهٔ IP معمولی از تونل رد می‌شود؛ بنابراین **TCP-in-TCP و فروپاشی بازارسال زیر اتلاف وجود ندارد**. چند حامل به این دلیل هست که throttle هر ۵-تایی (DPI) هر حامل را جدا محدود می‌کند و N حامل تا N برابر سقف را حمل می‌کنند.
- **نقش‌ها:** `edge` = ایران = `mode: "dial"` (کاربران، forward_ports)؛ `exit` = خارج = `mode: "listen"` (panel/expose) (`cmd/hs2/main.go:855`). اینکه **چه کسی حامل‌ها را dial می‌کند** با `engineDialsTransport` تعیین می‌شود: `(Mode=="dial") != Reverse` (`cmd/hs2/main.go:907`). پس چهار نقش اجرایی داریم:

| نقش | dial/accept | اندازه‌گیری (autopilot) | تابع اصلی | Phase در وضعیت |
|---|---|---|---|---|
| direct edge (ایران) | dial | بله: `autoscale` | `RunDgEdge` → `runLoop(direct=true)` (`engine/dgpool.go:2187-2232`, `2285-2307`) | فاز autopilot |
| direct exit (خارج) | accept | خیر | `RunDgExit` → حلقهٔ `directExitTick` (`engine/dgpool.go:2254-2274`) | `listening` |
| reverse edge (ایران) | accept | بله: `decide` + `publishTarget` + `reconcileReverseEdge` | `RunDgEdge` (`engine/dgpool.go:2217-2219`, `2296-2302`) | فاز autopilot |
| reverse exit (خارج) | dial | خیر؛ هدف را از edge می‌گیرد | `RunDgExit` → `runReverseExit` (`engine/dgpool.go:2276-2281`, `2310-2337`) | `following` |

- **مسیر کامل یک اتصال کاربر (TCP):** کاربر → `<user_listen_ip>:P` روی edge (forwarder در `dgports.go`) → یک **اتصال TCP جدید** از edge به `<peer_tun_ip>:28443` (بدون برچسب) یا `:28444` (با برچسب پورت) → هستهٔ edge آن را به TUN مسیریابی می‌کند → `pumpTun` → انتخاب حامل → صف عادلانه → `writeLoop` → `udpcarrier.Conn` (Noise + FEC + pacer + encap) → سیم → `udpcarrier.Conn` طرف مقابل → `readLoop` → `reorderer` → `tunBatch` → TUN خارج → هستهٔ خارج → listener روی `<local_tun_ip>:28443/28444` → dial به panel (`engine/dgforward.go:14-38`, `engine/dgports.go:16-49`).
  - یعنی هر اتصال کاربر **سه پای TCP** دارد (کاربر↔edge، edge↔exit از داخل تونل، exit↔panel)؛ پای میانی یک TCP مستقل به‌ازای هر کاربر است با بافر دریافت ثابت ۴ مگابایت (`dgTunRcvBuf`، `engine/dgforward.go:48-83`).
- **ثابت مهم موتور:** TUN یک بار باز می‌شود (با TCP offload، `cmd/hs2/main.go:836-851`) و به‌خاطر مرگ یک حامل هرگز بسته نمی‌شود؛ حامل‌ها زیر آن می‌آیند و می‌روند (`engine/dgpool.go:40-42`).
- **encapها:** udp/icmp/gre/ipip/ipx از طریق `EncapConfig{Kind, BindIP, Proto}` (`cmd/hs2/main.go:853`, `engine/dgcarrier.go:13-52`). سقف پیش‌فرض پول روی icmp برابر **۸** است (`icmpMaxLinks`، `cmd/hs2/main.go:671-678`, `cmd/hs2/main.go:711-724`).
- **MTU پیش‌فرض dgtun:** `1280` (`cmd/hs2/main.go:831-834`)؛ در l3mtcp `1380` (`cmd/hs2/main.go:458-461`).

---

## ۲. اجزای اصلی

### ۲.۱ نوع‌ها

| نوع | محل | نقش |
|---|---|---|
| `dgCarrier` (رابط) | `engine/dgpool.go:101-104` | `Carrier` + `Warm()`؛ `warmOf` اگر حامل نتواند بگوید، «گرم» فرض می‌کند (`:107-112`) |
| `DgDialer` / `DgListener` | `engine/dgpool.go:115-123` | dial/accept یک حامل |
| `dgLink` | `engine/dgpool.go:128-212` | یک حامل در پول: صف `fq`، writer اختصاصی، شمارنده‌ها، وضعیت retire/mute، نقشهٔ flowها، `ro` (reorderer) و `tb` (tunBatch) |
| `dgFlow` | `engine/dgpool.go:215-228` | فعالیت یک flow روی یک حامل: `last`، `bytes`، `ewma`، `steady` (۳ بیت)، `recv` (کلید روی بستهٔ دریافتی دیده شد) |
| `dgPool` | `engine/dgpool.go:480-579` | پول: `set []*dgLink`، `ap *autopilot`، `gate`، `target`، `sticky`، شمارنده‌های بسته، `gov *udpcarrier.Governor`، کانال برگشت آمار دانلود (`dnPressed/dnServing/dnStatsAt`)، `peerMax`، `burstLog`ها |
| `stickyFlow` | `engine/dgpool.go:581-584` | نگاشت flow → حامل فعلی + آخرین زمان |
| `dgFlowCount` | `engine/dgpool.go:1309-1324` | شمارش flow به تفکیک جهت؛ `conns()` = بیشینهٔ دو جهت |
| `dgDiag` / `sendDiag` | `engine/dgpool.go:1789-1838` | آمار مرحلهٔ ارسال بین دو نمونه |
| `stallGate` | `engine/dgpool.go:1067-1075` | قضاوت نکردن تیک‌هایی که دیر رسیده‌اند (توقف پروسه/VM) |
| `DgConfig` | `engine/dgpool.go:2156-2172` | پیکربندی یک طرف (Dev، Min/Max/PerLink، Reverse، Dialer/Listener، WarmLinks، OnStart) |
| `fqSched` / `fqFlow` / `fqList` | `engine/dgfq.go:76-176` | صف ارسال هر حامل: DRR + فهرست new/old |
| `qpkt` | `engine/l3_link.go:104-108` | بستهٔ صف‌شده + زمان صف (مشترک با l3) |
| `reorderer` | `engine/reorder.go:91-106` | بافر مرتب‌سازی TCP به‌ازای هر حامل |
| `tunBatch` | `engine/tunbatch.go:11-17` | جمع‌کردن نوشتن‌های TUN برای `WriteBatch` |
| `udpcarrier.Governor` | `udpcarrier/governor.go:13-97` | ناظر پول برای policer مسیر؛ سقف نرخ کل پول |
| `dgUDPDialer` / `dgUDPListener` | `engine/dgcarrier.go:18-52` | سازندهٔ حامل‌ها با `udpcarrier.DialCfg/ListenCfg` |
| `DgPortsConfig` / `dgExit` / `dgEdge` / `udpAddrs` | `engine/dgports.go:74-124`, `414-434` | forwarder پورت‌ها و مسیریابی هر پورت |
| `autoDialer` / `autoListener` / `udpDialer` / `udpListener` | `engine/carrier_udp.go:26-178` | حامل‌های تک‌جلسه‌ای `udp` و `auto` (مسیر Engine قدیمی؛ **در dgtun استفاده نمی‌شوند**) |

### ۲.۲ توابع کلیدی

| تابع | محل | کار |
|---|---|---|
| `newDgPool` | `engine/dgpool.go:586-611` | پیش‌فرض‌ها (min≥1، max≥min، perLink=8)، autopilot، governor، `target=warmSize` |
| `add` | `engine/dgpool.go:659-722` | نصب حامل، حذف zombieها، تشخیص spare، راه‌اندازی دو pump |
| `readLoop` / `onFrame` | `engine/dgpool.go:725-810` | دریافت قاب‌ها، دسته‌ای تا ۶۴، عبور از reorderer، دستورات کنترلی |
| `writeLoop` | `engine/dgpool.go:418-467` | تنها نویسندهٔ حامل؛ pop از fq، دور ریختن کهنه‌ها، خط سریع |
| `pumpTun` | `engine/dgpool.go:930-971` | خواندن TUN و قرار دادن بسته روی حامل؛ هرگز مسدود نمی‌شود |
| `pick` / `pickHash` / `pruneSticky` | `engine/dgpool.go:978-1043` | جای‌گذاری چسبنده + rendezvous چهارطبقه |
| `muteLoop` / `muteTick` | `engine/dgpool.go:1047-1142` | تشخیص حامل «بی‌صدا» |
| `sampleHealth` / `foldDownPressure` / `flowStats` | `engine/dgpool.go:1148-1305` | ساخت نمونهٔ autopilot |
| `autoscale` / `decide` / `reconcile` | `engine/dgpool.go:1328-1477` | تیک اندازه‌گیری direct |
| `scoutIfSilent` | `engine/dgpool.go:1342-1386` | dial پیشاهنگ وقتی همه ساکت‌اند |
| `queueDial` / `dialOne` / `wantsDial` | `engine/dgpool.go:1485-1534` | dial از طریق gate با epoch |
| `drainTick` | `engine/dgpool.go:1551-1600` | پاک‌سازی، بستن retiringهای خالی، یادآوری retire |
| `directExitTick` | `engine/dgpool.go:1542-1547` | تیک exit مستقیم |
| `reconcileReverseEdge` | `engine/dgpool.go:2346-2389` | retire/un-retire در edge معکوس |
| `onPoolCtl` / `onLinkStats` / `publishDownStats` / `publishTarget` / `publishInfo` / `poolCtlCarrier` | `engine/dgpool.go:1959-2151` | کنترل پول بین دو طرف |
| `publishStats` / `carrierStats` / `carrierLine` | `engine/dgpool.go:1672-1938` | وضعیت زنده |
| `echoBalance` / `carrierEchoShaped` | `engine/dgpool.go:887-925` | شکل‌دهی echo روی icmp |
| `closeLink` / `sendOp` / `onCloseFrame` | `engine/dgpool.go:824-871` | پیام‌های `TypeClose` |

### ۲.۳ goroutineها

| goroutine | چه کسی می‌سازد | نقش‌ها |
|---|---|---|
| `pumpTun` (یکی) | `RunDgEdge:2213`، `RunDgExit:2250` | همه |
| `reapLoop` (هر ۱ ثانیه) | `:2214`, `:2251` | همه |
| `muteLoop` (هر ۲۵۰ms) | `:2215`, `:2252` | همه |
| `gov.Run` (هر ۵۰۰ms) | `:2216`, `:2253`; `udpcarrier/governor.go:145-157` | همه |
| `acceptLoop` | `:2218` (reverse edge)، `:2259` (direct exit) | طرف accept |
| `publishTarget` | `:2219` | reverse edge |
| `publishInfo` | `:2221` | direct edge |
| `runLoop` / `runReverseExit` / حلقهٔ `directExitTick` | فراخوانی مستقیم (روی goroutine فراخواننده) | edge / reverse exit / direct exit |
| `writeLoop` + `readLoop` به‌ازای هر حامل | `add:705-706` | همه |
| تایمر `reorderer.expire` (AfterFunc) | `engine/reorder.go:306-350` (`arm`/`expire`) | به‌ازای هر حامل |
| goroutine هر dial (`queueDial`) و scout | `:1497-1500`, `:1375-1385` | طرف dial |
| `sendOp` ناهمگام (closeHear/closeMute) و `closeLink` با jitter | `:1113`, `:1135`, `:1139`, `:1598` | همه |
| در dgports: `probeLoop`، حلقه‌های accept، `serveTCP`/`serveUDP`، جاروب flowهای UDP هر ۳۰ ثانیه | `engine/dgports.go:447`, `256-279`, `326-344`, `744-762` | edge/exit |

---

## ۳. جریان داده و کنترل (گام‌به‌گام)

### ۳.۱ مسیر ارسال: TUN → سیم (`pumpTun`)
1. بافر از `sync.Pool` (ظرفیت پیش‌فرض ۲۰۴۸) یا ساخت تازه با اندازهٔ `MTU+128` (`engine/dgpool.go:931-937`, `602`). با offload، `tun.Device.Read` هر بار **یک segment** به اندازهٔ MTU برمی‌گرداند (`tun/tun_linux.go:155-180`).
2. `flowHash` (FNV-1a روی src/dst IP + proto + پورت‌ها برای TCP/UDP در IPv4؛ در IPv6 فقط دو آدرس) (`engine/l3_link.go:399-417`).
3. `pick(flow, now)` (`engine/dgpool.go:978-996`):
   - اگر flow در `sticky` است و حاملش زنده است، **کمتر از `flowletGap`=300ms** مکث کرده، `retireForced` نیست و `avoid` نیست → همان حامل (حتی اگر retiring باشد).
   - وگرنه از sticky حذف و `pickHash` (rendezvous: وزن `mix32(flow ^ l.id)`) با ۴ طبقه: ۰=serving، ۱=retiring یا peerRetiring، ۲=serving ولی avoid، ۳=retiring و avoid؛ اولین طبقهٔ غیرخالی (`engine/dgpool.go:1015-1043`). نتیجه در sticky ثبت می‌شود.
4. `enqueue` → `fq.push` (`engine/dgpool.go:366-377`):
   - صف پر (۲۵۶ بسته): سر صفِ **چاق‌ترین flow** دور ریخته می‌شود (ممکن است خود تازه‌وارد باشد) → `droppedAt` (فشار) + `NoteQueueDrop()` به مدل نرخ حامل. بستهٔ جابه‌جاشده بازیافت و در `dropQueueFull` شمرده می‌شود (`:958-968`).
   - `noteFlowSend` → `bytesUp` و رکورد flow (`:387-398`).
5. `writeLoop` (`engine/dgpool.go:418-467`):
   - `fq.pop` → اگر sojourn > `dgSojourn` (۵۰ms): دور ریختن + **ثبت فشار** (`droppedAt`) + `NoteQueueDrop` + `dropAged`.
   - اگر `urgent` و حامل `SendUrgent` دارد → خط سریع pacer؛ وگرنه `SendFrame(TypeData)`.
   - خطای ارسال → `lose(err)` → حامل مرده.
   - برای بستهٔ غیرفوری: `fq.noteSlow(flow, LaneMark())`.
   - شمارش `sentPkts` و برای icmp `txFrames`.

### ۳.۲ مسیر دریافت: سیم → TUN (`readLoop`)
1. `ReadFrame()` مسدودکننده (حامل خودش بعد از `deadAfter`=۱۵s خطا می‌دهد، `udpcarrier/carrier.go:309-327`).
2. تا `dgReadBatch`=۶۴ قاب دیگر با `TryReadFrame` بدون انتظار (`engine/dgpool.go:743-757`).
3. `onFrame` برای `TypeData`: `recvPkts`، `noteFlowRecv(flowHash)` (کلید جهت دریافت)، سپس `ro.Push` (reorderer) → `tb.add` → در پایان دسته `tb.flush()` با `WriteBatch` (`engine/dgpool.go:779-792`, `758-760`).
4. reorderer (`engine/reorder.go:197-279`؛ `Push` از خط `200`): فقط TCP دارای داده و بدون fragment نگه داشته می‌شود؛ segment جلوتر از شکاف حداکثر `dgReorderHold`=15ms صبر می‌کند؛ SYN/RST حالت flow را پاک می‌کند؛ بازارسال‌ها و ACKهای خالی و غیر-TCP مستقیم رد می‌شوند. سرریز: ۲۵۶ بسته در هر flow یا ۴۰۹۶ در هر حامل → رهاسازی همه.
5. tunBatch با offload، segmentهای متوالی یک اتصال را با قواعد GRO یکی می‌کند (`engine/tunbatch.go:5-10`).
6. قاب‌های دیگر: `TypeClose` → `onCloseFrame`؛ `TypePing/TypePong` بی‌اثر (فقط پرکنندهٔ echo)؛ `TypePoolCtl` → `onPoolCtl`؛ `TypeLinkStats` → `onLinkStats` (`engine/dgpool.go:793-805`). در پایان، برای icmp `echoBalance`.

### ۳.۳ چرخهٔ عمر حامل
1. **ساخت:** `dialOne` بعد از نوبت در `dialGate` (سراسری، ۸ هم‌زمان، فاصلهٔ ۴۰–۱۶۰ms؛ `engine/dialgate.go:22-41`, `engine/linkmanager.go:394-396`) و بررسی دوبارهٔ `valid` (epoch یکسان و `wantsDial`) (`engine/dgpool.go:1505-1534`). یا `acceptLoop` (`:2392-2405`).
2. **`add`** (`engine/dgpool.go:659-722`): تشخیص icmp و نقش echo، اتصال به governor، ساخت `tb` و `ro` **پیش از** انتشار در `p.set` (رفع data race)، شناسایی **zombieها** (حامل‌هایی که ≥۳s چیزی نشنیده‌اند) و حذفشان چون حامل تازه ثابت کرده مسیر کار می‌کند؛ zombieها در شمارش serving لحاظ نمی‌شوند. در **reverse edge** اگر serving ≥ target باشد حامل «spare» متولد می‌شود: `retiring=true, bornSpare=true` و بلافاصله `closeRetire` به طرف مقابل.
3. **زندگی:** serving یا retiring؛ ممکن است muted یا peerMute شود.
4. **مرگ:** (الف) خطای ارسال/خواندن → `lose` با دلیل → لاگ «failed»؛ (ب) `closeBye` از طرف مقابل؛ (ج) `closeLink` عمدی (دو بار `closeBye` و سپس بستن)؛ (د) mute ≥ ۳s → `closeLink`؛ (ه) zombie هنگام آمدن حامل تازه؛ (و) `deadAfter`=۱۵s داخل حامل. `reapLoop` هر ثانیه مرده‌ها را از `set` حذف می‌کند (`:2408-2427`).

### ۳.۴ تیک اندازه‌گیری (هر `healthTick` = ۲s)
- **direct edge** (`autoscale`، `engine/dgpool.go:1328-1334`): `sampleHealth` → `decide` (autopilot مشترک؛ `engine/autopilot.go:340`) → `reconcile(T)` → `scoutIfSilent` → `publishStats`؛ سپس `drainTick` (`:2293-2304`).
- **reverse edge:** `sampleHealth` → `decide` → `reconcileReverseEdge` → `publishStats` → `drainTick` (`:2296-2304`). هدف با `publishTarget` جدا ارسال می‌شود.
- **reverse exit** (`runReverseExit`، `:2310-2337`): اگر هیچ حاملی نیست و قبلاً حامل داشته و target > warm → target به warm برمی‌گردد (edge تازه‌راه‌اندازی‌شده نباید هدف قدیمی را به‌صورت spare تحویل بگیرد)؛ `reconcile(Target)` (فقط dial، چون `revExit`)؛ `scoutIfSilent`؛ `drainTick`؛ `sampleHealth` → `publishDownStats` → `publishStats`.
- **direct exit** (`directExitTick`، `:1542-1547`): `pruneSticky` → `sampleHealth` → `publishDownStats` → `publishStats` (بدون autopilot، بدون drainTick).

**`sampleHealth`** (`engine/dgpool.go:1148-1211`) برای هر حامل زنده:
- `expirePeerRetire` (اعلان retire طرف مقابل که ۱۲s تمدید نشده)؛
- `rate` = (Δup+Δdown)/dt؛ `rates[5]` → `rate10` (میانگین ۵ نمونه = ۱۰s)؛ `doms[3]` → `sustained` = کمینهٔ ۳ نمونهٔ جهت غالب؛
- `flowStats` → `flowing`/`open` با قاعدهٔ مشترک mtcp: EWMA ≥ `flowingRate` (۲KB/s) یا ۳ نمونهٔ پیاپی ≥ `flowSteadyRate` (۲۵۶B/s)؛ flowهای قدیمی‌تر از `flowRecent` (۶s) حذف؛ شمارش به تفکیک جهت و `conns()`=بیشینهٔ دو جهت (`:1265-1324`)؛
- **فشار:** `pressed = !capped && !retiring && warm && droppedAt در همین تیک` (`:1191-1198`)؛ «گرم» یعنی `Warm()` حامل یا گذشت `dgWarmGrace`=۴s از `servingSince`؛ زیر سقف governor هیچ فشاری شمرده نمی‌شود؛
- در پایان `foldDownPressure`.

### ۳.۵ کوچک‌شدن (retire) و آینه‌کردن آن در طرف مقابل
1. `reconcile` (direct edge): اگر `need<0` و نه `revExit`، به تعداد لازم حامل‌های «زودتر خالی‌شونده» (کمترین flow اخیر، سپس کمترین `rate10`؛ `sortByEmptiest`، `engine/dgpool.go:1643-1669`) retiring می‌شوند و `closeRetire` می‌فرستند (`:1449-1467`). رشد: ابتدا busiest retiringها un-retire (`closeServe`)، بقیه dial با بودجهٔ `min(max(4, ceil(T/16)), 20)` و سقف `max - count - dialing` (`:1469-1476`).
2. طرف مقابل در `onCloseFrame`: `peerRetireAt` و `retiringAt` تنظیم می‌شوند؛ `pickHash` حامل را طبقهٔ ۱ می‌گذارد (flowlet جدید نمی‌گیرد) (`:854-857`, `:1027-1029`).
3. flowهای چسبیده تا مکث ≥300ms روی حامل می‌مانند؛ بعد از `dgRetireForce` (۳۰s) حتی بدون مکث جابه‌جا می‌شوند (`pick`، `:981`).
4. `drainTick` (`:1551-1600`): حامل retiring بدون flow فعال در ۳۰۰ms اخیر، با شرط `bornSpareGrace` (۳۰s برای spare) و `dgRetireHold` (۶s فقط در reverse edge)، با jitter ۵۰–۲۵۰ms بسته می‌شود. retiringهای زنده هر ≥۳s یک `closeRetire` یادآوری می‌فرستند.
5. اگر `closeServe` گم شود، اعلان retire در طرف مقابل بعد از ۱۲s تمدیدنشده منقضی می‌شود (`expirePeerRetire`، `:284-293`).
6. **reverse:** فقط edge تصمیم retire می‌گیرد؛ exit هرگز خودش retire نمی‌کند، فقط بالای target dial نمی‌کند (`revExit`، `:484-488`, `:1449`). edge با `reconcileReverseEdge` حامل‌های اضافه را retire و با بالا رفتن target ابتدا retiring/spareها را دوباره serving می‌کند (`:2346-2389`).

### ۳.۶ تشخیص حامل «بی‌صدا» (mute)
- هر ۲۵۰ms (`muteLoop`)، با `stallGate` که تیک دیرتر از ۲ دوره را و ۵۰۰ms بعدش را قضاوت نمی‌کند (`:1047-1075`).
- `muteTick` (`:1083-1142`): حامل با `lastRx` صفر قضاوت نمی‌شود. «heard» = حامل‌هایی که کمتر از ۱s پیش شنیده‌اند.
  - اگر **هیچ** حاملی نشنیده: هیچ قضاوتی نمی‌شود و همهٔ علامت‌ها برداشته می‌شوند (کار scout است).
  - حامل ساکت ≥۱s در حالی که دیگری می‌شنود → `muted=true`، لاگ، و **دو بار** `closeMute` به طرف مقابل (ناهمگام).
  - حامل muted که ≥ `dgSilentDead` (۳s) ساکت مانده → یک‌باره (`muteClosed` CAS) `closeLink` و شمارنده `muteClosed`. (اگر حامل اولین بار با سن ≥۳s دیده شود، در این تیک فقط mute می‌شود و در تیک بعد بسته می‌شود.)
  - حامل muted که دوباره شنیده → برداشتن علامت و `closeHear`.
- طرف مقابل: `peerMuteAt` تنظیم می‌شود و `avoid()` تا `dgPeerMuteFor` (۴s) یا رسیدن `closeHear` برقرار است (`:303-309`, `:861-868`). یعنی **قطع یک‌طرفه در هر دو سو ترمیم می‌شود**.
- حامل کنترل پول هم از حامل avoid استفاده نمی‌کند (`poolCtlCarrier`، `:2122-2151`).

### ۳.۷ ساکت شدن همه، scout و zombie
- `scoutIfSilent` (`:1342-1386`): فقط طرف dial، وقتی هیچ dialی در جریان نیست و **همهٔ** حامل‌های زنده ≥۳s ساکت‌اند، حداکثر هر ۵s یک حامل پیشاهنگ dial می‌شود (لاگ فقط یک بار برای هر دورهٔ سکوت). اگر بالا بیاید، `add` بقیهٔ ساکت‌ها را به‌عنوان zombie می‌کشد و پول بلافاصله پر می‌شود.
- اگر هیچ حاملی وجود نداشته باشد (`n==0`) scout کاری نمی‌کند؛ پر کردن با `reconcile` معمولی است.

### ۳.۸ کانال برگشت فشار دانلود
- مشکل (`engine/dgpool.go:522-529`): edge اندازه را تعیین می‌کند ولی گیرندهٔ دانلود است و فشار صف ارسال فرستندهٔ دانلود (exit) را نمی‌بیند؛ پولِ دانلودمحور تا min کوچک می‌شد.
- exit (`downSender=true`) در هر تیک `publishDownStats`: `[pressed u16][serving u16][ceiling u16]` روی یک حامل (`:2029-2052`).
- edge در `onLinkStats` ذخیره می‌کند (`:1986-1996`) و `foldDownPressure` (`:1224-1263`): اگر گزارش تازه (≤۶s) است، `want - have` حامل serving غیرفشرده‌ای را که در این تیک دانلود داشته‌اند، **به ترتیب بیشترین نرخ دانلود** فشرده علامت می‌زند؛ هرگز حامل بیکار را فشرده نمی‌کند؛ فشار مؤثر = max(بالا، پایین) بدون شمارش دوباره.
- چون شناسهٔ حامل در دو سر متفاوت است (`randSeed` جداگانه در `newDgLink`)، نگاشت حامل‌به‌حامل وجود ندارد و فقط **تعداد** منتقل می‌شود.

### ۳.۹ forward پورت‌ها و مسیریابی هر پورت (`dgports.go`)
- **exit** (`startDgExit`، `engine/dgports.go:162-196`): اگر `expose` هست، forwarder TCP روی `<local_tun_ip>:28443` → panel با بافر ۴MB روی پای تونل، و همیشه UDP روی همان آدرس (خطای UDP فقط وقتی `udp: true` باشد کشنده است). پورت برچسب‌دار ۲۸۴۴۴ همیشه باز می‌شود (حتی بدون جدول) تا probe بتواند پاسخ دهد؛ شکستش فقط مسیریابی هر پورت را خاموش می‌کند.
  - `serveTCP` (`:284-313`): سرآیند `[1][port u16]` با مهلت `kindTimeout` (۱۰s)؛ `port==0` = probe: خواندن `peerInfo` edge و پاسخ با `mine()` (Caps=`capPortTags`، Ports=نگاشت‌شده‌ها، Flags=`flagDefault` اگر expose هست)؛ وگرنه `table.Target(port)` → `proxyTCP`، یا ثبت «no target» حداکثر یک بار در دقیقه برای هر پورت/پروتکل.
  - `serveUDP` (`:318-410`): کلید flow = آدرس مبدأ edge؛ هدف از برچسب اولین datagram؛ اگر همان آدرس مبدأ برای پورت دیگری استفاده شود، flow قدیمی جایگزین می‌شود؛ پاسخ‌ها بدون برچسب برمی‌گردند؛ نام هدف‌ها یک بار resolve و کش می‌شود (`udpAddrs`، تلاش دوباره بعد از ۳۰s).
- **edge** (`startDgEdge`، `:436-456`): `probeLoop` حتی بدون پورت کاربر؛ برای هر پورت listener TCP (و UDP اگر `udp`).
  - **ماشین حالت probe** (`probe`، `:519-579`): dial به ۲۸۴۴۴ با مهلت ۳s:
    - refuse → `older()` (no)؛
    - timeout: اگر ۲۸۴۴۳ هم پاسخ نمی‌دهد → unknown (تونل بالا نیست)؛ اگر پاسخ می‌دهد → یک بار دیگر ۲۸۴۴۴؛ اگر باز ساکت: اگر حالت فعلی yes و این اولین سکوت است → unknown (flap)؛ وگرنه **filtered** (no) با لاگ یک‌باره؛
    - پذیرفت ولی در `dgProbeAnswer` (=`infoTimeout` ۵s) پاسخ نداد → `undecided=true`، unknown (چیزی برچسب نمی‌خورد، اتصال‌ها منتظر نمی‌مانند)؛
    - پاسخ غیر hs2 → older؛
    - پاسخ hs2 با `capPortTags` → yes؛ `udpTag = flagCut || !flagDefault || len(Ports)>0`.
  - فاصلهٔ probe: ۱۵s؛ در unknown هر ۲s؛ تا ۲ دقیقه بعد از تبدیل به no هر ۵s؛ با `poke` فوری (`:489-505`).
  - **TCP** (`serveTCP`، `:707-732`): در حالت unknown تا ۱۰s منتظر probe (مگر undecided)؛ در yes همیشه برچسب‌دار (هر اتصال ۳ بایت)؛ refuse روی ۲۸۴۴۴ → older + poke + بازگشت به ۲۸۴۴۳ **قبل از ارسال هر بایت کاربر**؛ خطای غیر refuse → بستن کاربر + poke.
  - **UDP** (`serveUDP`، `:736-838`): یک سوکت بالادست برای هر کلاینت؛ برچسب فقط وقتی `tagUDP` (yes و exit جدول دارد)؛ اگر وضعیت برچسب عوض شود، flow از datagram بعدی از نو ساخته می‌شود؛ refuse روی flow برچسب‌دار فقط probe را poke می‌کند.
- **forwarder عمومی** (`dgforward.go`): `runForwarder` (`:93-130`)، `proxyTCP` با `forwardDialer` (مهلت ۱۰s، تنظیم/قفل rcvbuf پیش از connect تا SYN مقیاس پنجرهٔ درست را اعلام کند، `:132-154`)، `proxyUDP` با بی‌کاری ۹۰s (`:156-237`)، `relay` با بافرهای ۳۲KB (`engine/stream.go:53`). listenerها MPTCP را خاموش می‌کنند (`engine/listen.go:41`).

### ۳.۱۰ شکل‌دهی echo روی icmp (B5)
- `carrierEchoShaped`: فقط وقتی `Encap()` پس از trim و بی‌توجه به حروف «icmp» است (`:887-894`). `echoDial` = این طرف dial می‌کند → درخواست echo می‌فرستد.
- `echoBalance` (`:912-925`) به‌ازای هر قاب **واقعی** دریافتی (نه Ping/Pong): اگر `txFrames < rxFrames`، یک پرکنندهٔ ارزان (`TypePing` در طرف dial، `TypePong` در طرف listen) با padding تصادفی ۰..۹۵ بایت صفر. جهت سنگین هرگز پر نمی‌کند و پرکننده هرگز پرکننده تولید نمی‌کند.

---

## ۴. جدول ثابت‌ها، آستانه‌ها، بافرها و زمان‌سنج‌ها

| نام | مقدار | محل | معنی |
|---|---|---|---|
| `dgQueueLen` | 256 بسته | `engine/dgpool.go:49` | سقف صف ارسال هر حامل (fq) |
| `dgSojourn` | 50ms | `engine/dgpool.go:50` | بیشینهٔ انتظار در fq؛ بیشتر = دورریز + فشار |
| `dgWarmGrace` | 4s | `engine/dgpool.go:54` | فشار حامل تازه تا این مدت شمرده نمی‌شود (مگر `Warm()`) |
| `dgPoolCtlEvery` | 3s | `engine/dgpool.go:56` | دورهٔ ارسال target در reverse و یادآوری retire |
| `dgSilentDead` | 3s | `engine/dgpool.go:61` | سکوت = مرگ (zombie/scout/بستن mute) |
| `dgScoutEvery` | 5s | `engine/dgpool.go:64` | فاصلهٔ dial پیشاهنگ |
| `dgMuteAfter` | 1s | `engine/dgpool.go:74` | سکوت لازم برای mute (≈ ده گزارش ۱۰۰ms) |
| `dgMuteEvery` / `dgMuteHold` | 250ms / 500ms | `engine/dgpool.go:79-80` | دورهٔ mute watch / مکث پس از تیک دیررس |
| `dgPeerMuteFor` | 4s (=3s+1s) | `engine/dgpool.go:85` | اعتبار `closeMute` طرف مقابل |
| `dgInfoEvery` | 15s (±20%) | `engine/dgpool.go:89`, `:2080` | ارسال سقف در direct edge |
| `dgPeerMaxStale` | 45s | `engine/dgpool.go:90` | کهنگی سقف طرف مقابل (فقط نمایش) |
| `dgDialBudget` | 4 | `engine/dgpool.go:91` | پایهٔ بودجهٔ dial در هر تیک (`min(max(4,⌈T/16⌉),20)`، `:1471`) |
| `dgDownStatsStale` | 6s (=3×healthTick) | `engine/dgpool.go:95` | کهنگی گزارش فشار دانلود |
| `dgPeerRetireStale` | 12s (=4×3s) | `engine/dgpool.go:280` | انقضای اعلان retire تمدیدنشده |
| `dgRetireForce` (var) | 30s | `engine/dgpool.go:347` | پس از آن flowهای چسبیده از حامل retiring جابه‌جا می‌شوند |
| `closeBye..closeHear` | 0..4 | `engine/dgpool.go:355-359` | opهای `TypeClose` |
| `echoFillerPad` | 96 بایت صفر | `engine/dgpool.go:880` | padding پرکنندهٔ echo (۰..۹۵) |
| `dgReadBatch` | 64 | `engine/dgpool.go:765` | قاب در هر گذر readLoop |
| `dgDialFailRun` | 3 | `engine/dgpool.go:1481` | شکست پیاپی dial پیش از رها کردن dialهای صف‌شده |
| `dgRetireHold` | 6s (=2×3s) | `engine/dgpool.go:1604` | نگه‌داشت retiring در reverse edge پیش از بستن |
| `flowletGap` | 300ms | `engine/dgpool.go:1608` | مکثی که پس از آن flow دوباره hash می‌شود؛ معیار «flow فعال» در drain |
| `carrierLineMax` / `carrierLineWorst` | 32 / 10 | `engine/dgpool.go:1942-1943` | خلاصه‌سازی خط carriers |
| آستانهٔ حامل فعال در آمار FEC | rate ≥ 1000 B/s | `engine/dgpool.go:1719` | حامل بیکار در میانگین parity/loss شمرده نمی‌شود |
| hysteresis لاگ FEC | ۲ نمونه بالا / ۵ نمونه پایین | `engine/dgpool.go:1761-1782` | جلوگیری از flap لاگ |
| poll در `publishTarget` | 200ms | `engine/dgpool.go:2113` | ارسال فوری تغییر target |
| مکث acceptLoop | 50ms | `engine/dgpool.go:2400` | پس از خطای Accept |
| `reapLoop` | 1s | `engine/dgpool.go:2409` | حذف مرده‌ها از set |
| بافر بسته | cap 2048؛ خواندن MTU+128 | `engine/dgpool.go:602`, `:931` | |
| `healthTick` | 2s | `engine/health.go:18` | تیک اندازه‌گیری مشترک |
| `flowTau` / `flowingRate` / `flowRecent` / `flowSteadyRate` | 10s / 2KB/s / 6s / 256B/s | `engine/health.go:41-48` | قاعدهٔ «flowing» مشترک با mtcp |
| `warmStartLinks` | 8 | `engine/health.go:85` | اندازهٔ شروع (`warmSize`، `engine/linkmanager.go:742-752`) |
| `bornSpareGrace` (var) | 30s | `engine/linkmanager.go:308` | عمر حداقل حامل spare |
| `dialFailLogEvery` | 30s | `engine/linkmanager.go:106` | فاصلهٔ لاگ شکست dial |
| `closeJitter` | 50–250ms | `engine/linkmanager.go:399-401` | فاصله‌گذاری بستن‌ها |
| `gateInflight` / `jitterGap` | 8 / 40–160ms | `engine/dialgate.go:22`, `engine/linkmanager.go:394-396` | gate سراسری dial |
| `burstLines` / `burstWin` | 8 / 10s | `engine/burstlog.go:18-19` | تا شدن لاگ‌های پرتکرار |
| `fqQuantum` | 1500 بایت | `engine/dgfq.go:49` | سهم هر نوبت DRR و head start |
| `fqUrgentGap` | 100ms | `engine/dgfq.go:54` | جایگزین ترتیب برای حاملی بدون LaneMark |
| `fqForget` | 2s | `engine/dgfq.go:57` | فراموشی رکورد flow بیکار (جاروب حداکثر هر ۱s، `:323-333`) |
| `fqSparseRate` / `fqSparseBurst` | 32000 B/s (۲۵۶kbit/s) / 8KB | `engine/dgfq.go:62-63` | سطل توکن «flow تُنُک» |
| فشرده‌سازی آرایهٔ flow | head≥64 و 2·head≥len | `engine/dgfq.go:123-129` | جلوگیری از رشد بی‌حد |
| `dgReorderHold` | 15ms (env) | `engine/reorder.go:49-56` | نگه‌داشت پشت شکاف |
| `reorderFlowMax` / `reorderTotalMax` / `reorderIdle` / `reorderSweep` | 256 / 4096 / 60s / 4096 push | `engine/reorder.go:59-62` | سقف‌های reorderer |
| `DgTunPort` / `DgTagPort` | 28443 / 28444 | `engine/dgforward.go:46`, `engine/dgports.go:53` | پورت‌های روی تونل |
| `dgTunRcvBuf` | 4MB (env) | `engine/dgforward.go:76-83` | rcvbuf پای تونل |
| `forwardDialer.Timeout` | 10s | `engine/dgforward.go:147` | مهلت dial forwarder |
| `udpProxyIdle` | 90s؛ جاروب ۳۰s | `engine/dgforward.go:158`, `:174` | بی‌کاری flow UDP |
| `dgTagVer` | 1 | `engine/dgports.go:56` | نسخهٔ سرآیند برچسب |
| `dgProbeEvery` / `dgProbeRetry` / `dgProbeUnknown` | 15s / 5s / 2s | `engine/dgports.go:57-62` | فاصلهٔ probe |
| `dgPeerStale` | 45s | `engine/dgports.go:59` | کهنگی گزارش edge در exit |
| `dgFastAfterNo` | 2min | `engine/dgports.go:60` | دورهٔ probe سریع پس از no |
| `dgProbeDial` / `dgUnknownWait` | 3s / 10s | `engine/dgports.go:61-63` | مهلت dial probe / انتظار اتصال برای اولین probe |
| `dgProbeAnswer` (=`infoTimeout`) | 5s | `engine/dgports.go:635`, `engine/peerinfo.go:52` | مهلت پاسخ probe |
| `udpAddrRetry` | 30s | `engine/dgports.go:125` | تلاش دوبارهٔ resolve |
| `kindTimeout` | 10s | `engine/stream.go:51` | مهلت خواندن سرآیند برچسب در exit |
| `noRouteEvery` | 1min | `engine/routes.go:130` | لاگ «no target» |
| `maxRecoverableLoss` | 0.45 | `engine/carrier_udp.go:24` | فقط حامل `auto` (نه dgtun) |
| probe حامل `auto` | 16 بسته، 8ms، 300ms | `engine/carrier_udp.go:70` | فقط `auto` |
| `deadAfter` / `feedbackEvery` حامل | 15s / 100ms | `udpcarrier/carrier.go:54-55` | مبنای آستانه‌های mute/silent |
| `camoIdlePoll` | 700ms (±20%) | `udpcarrier/carrier.go:39`, `:566-570` | ضربان بیکار زیر `HS2_ICMP_CAMO=1` |
| handshake dial حامل | تا 10s، 12 تلاش؛ confirm 6s | `udpcarrier/dial.go:190-198`, `:126` | هزینهٔ یک dial ناموفق |
| `icmpMaxLinks` | 8 | `cmd/hs2/main.go:678` | سقف پیش‌فرض روی icmp |
| min/per پیش‌فرض | 2 / 8 | `cmd/hs2/main.go:644-656` | `linkEnvelope` |
| MTU پیش‌فرض | 1280 | `cmd/hs2/main.go:831-834` | dgtun |
| ثابت‌های governor | تیک 500ms، تشخیص 30s، `govCapFrac` 0.9، کف 1Mbit/s، ... | `udpcarrier/governor.go:170-195` | (زیرسیستم udpcarrier) |

---

## ۵. حلقه‌های کنترلی

| حلقه | دوره | ورودی | شرط | خروجی |
|---|---|---|---|---|
| `runLoop` (edge) | 2s | `sampleHealth` | — | `decide` → target؛ direct: `reconcile` + scout؛ reverse: `reconcileReverseEdge`؛ سپس `drainTick` |
| autopilot `decide` | هر تیک | `apSample` (links: serving/retiring/pressed/rate/rate10/sustained/flowing/open؛ G؛ flowing؛ open؛ growable=true) | قواعد مشترک (کف از flowهای فعال، رشد probe-محور فقط با فشار و کمبود spare، کوچک‌شدن پس از ۶۰s) (`engine/autopilot.go:11-42`, `340-...`) | `apDecision{target, phase, reason, note}` |
| `reconcile` | هر تیک | T | `need = T - serving` | un-retire busiest / dial (بودجه) / retire emptiest + `closeRetire/closeServe` |
| `drainTick` | هر تیک (edge، reverse exit) | flowهای retiring | بدون flow ≤300ms + `bornOK` + `heldOK` | بستن با jitter؛ یادآوری `closeRetire` هر ≥3s |
| `runReverseExit` | 2s | target از edge | — | dial تا target، scout، drain، گزارش فشار دانلود |
| `directExitTick` | 2s | — | — | prune sticky، گزارش فشار دانلود، وضعیت |
| `muteLoop` | 250ms | `LastRx` همه | `stallGate` | mute/hear/close + اطلاع به طرف مقابل |
| `scoutIfSilent` | در هر تیک | `silent()` همه | همه ساکت، dial در جریان نیست، ≥5s از قبلی | یک dial پیشاهنگ |
| `reapLoop` | 1s | `alive()` | — | حذف مرده‌ها |
| `publishTarget` | 3s + تغییر (poll 200ms) | `Target()` | حامل کنترل موجود | `TypePoolCtl` |
| `publishInfo` | 1s تا اولین حامل، سپس 15s±20% | — | — | `TypePoolCtl` (فقط سقف برای نمایش) |
| `publishDownStats` | هر تیک exit | s.links | — | `TypeLinkStats` |
| `gov.Run` | 500ms | آمار حامل‌ها | الگوی رخدادهای اتلاف هم‌زمان | سقف نرخ کل پول (token bucket مشترک) |
| `reorderer.expire` | AfterFunc برابر hold | قدیمی‌ترین بستهٔ نگه‌داشته | ≥15ms | رهاسازی + `flush` |
| `fqSched.sweep` | حداکثر 1s (در pop) | رکورد flowها | بیکار ≥2s و خط داده تخلیه‌شده | حذف رکورد |
| `probeLoop` (dgports) | 15s / 5s / 2s / poke | probe | — | وضعیت برچسب |
| جاروب flow UDP | 30s | `last` | بیکار >90s | بستن سوکت بالادست |

---

## ۶. حالت‌ها و گذارها، خطاها و بازیابی

### ۶.۱ حالت‌های یک حامل
- **serving** (پیش‌فرض)، **retiring** محلی (`l.retiring` زیر `p.mu`)، **peerRetiring** (`peerRetireAt != 0`)، **retiringAt** (اولین لحظهٔ retire از هر سو؛ مبنای `retireForced`)، **bornSpare** (فقط reverse edge)، **muted** (محلی)، **peerMute** (`peerMuteAt`، تا ۴s)، **dead** (`dead` + `done` بسته + `lostErr` در صورت شکست).
- گذارها:
  - serving → retiring: `reconcile` / `reconcileReverseEdge` (+ `closeRetire`)؛
  - retiring → serving: رشد target (+ `closeServe`)؛ در طرف مقابل: `closeServe` یا انقضای ۱۲s؛
  - serving → muted: سکوت ≥1s با شنیدن دیگران؛ muted → serving: شنیدن دوباره؛ muted → dead: سکوت ≥3s؛
  - هر حالت → dead: خطای I/O، `closeBye`، zombie، `closeLink`، `closeAll` هنگام توقف.
- `countsLocked` فقط `l.retiring` محلی را retiring می‌شمارد؛ حامل peerRetiring در شمارش serving همان طرف است (`engine/dgpool.go:642-654`).

### ۶.۲ حالت‌های edge در مسیریابی هر پورت
`dgTagUnknown` → `dgTagYes` | `dgTagNo` (با زیرحالت `filtered` یا `older`)؛ پرچم‌های `undecided` و شمارندهٔ `silent` (`engine/dgports.go:67-71`, `414-434`, `583-627`). no → yes با probe موفق بعدی؛ filtered → older با یک refuse.

### ۶.۳ جدول خطا و بازیابی

| رخداد | تشخیص | واکنش | زمان تقریبی |
|---|---|---|---|
| یک حامل قطع (هر دو جهت) | mute | جابه‌جایی flowها در بستهٔ بعدی؛ بستن در ۳s؛ جایگزینی | ~1s جابه‌جایی |
| قطع یک‌طرفه | mute در یک سو + `closeMute` | سوی دیگر هم avoid می‌کند | ~1s |
| ری‌استارت طرف مقابل (بدون bye) | silent ≥3s | حامل تازه/scout → حذف zombieها | چند ثانیه |
| قطع کامل مسیر | همه silent | scout هر 5s (طرف dial)؛ mute قضاوت نمی‌شود | — |
| توقف عمدی | `closeBye` ×2 | مرگ فوری در طرف مقابل | فوری |
| شکست dial | `failStreak≥3` یا count==0 | `dialEpoch++` (dialهای صف‌شده رها)، لاگ هر 30s | تیک بعد دوباره |
| گم شدن `closeServe` | عدم تمدید retire | انقضا پس از 12s + لاگ | 12s |
| گم شدن `closeHear` | — | انقضای peerMute پس از 4s | 4s |
| VM/پروسه متوقف | `stallGate` | قضاوت mute نمی‌شود تا ۵۰۰ms پس از تیک دیررس | — |
| گزارش دانلود قطع | کهنگی 6s | بازگشت به اندازه‌گیری فقط بالارو | 6s |
| edge ری‌استارت (reverse exit) | `count()==0 && hadCarrier` | target به warm برمی‌گردد | تیک بعد |
| policer مسیر | governor | سقف کل؛ فشار شمرده نمی‌شود | (udpcarrier) |
| خطای Accept موقت در forwarder | `acceptBackoff` | ادامه، لاگ حداکثر یک بار در دقیقه (`engine/engine.go:222-233`) | — |
| پورت ۲۸۴۴۴ فیلتر | probe دوبار ساکت | بدون برچسب + لاگ | ≤ یک دورهٔ probe |

---

## ۷. پیام‌های پروتکل و قالب قاب‌ها

همهٔ قاب‌ها داخل نشست Noise حامل مهروموم می‌شوند؛ داده با FEC و pacer، کنترل به‌صورت datagram خام کنترلی (`udpcarrier/carrier.go:228-236`, `287-301`).

| نوع | شماره | جهت | محموله | محل |
|---|---|---|---|---|
| `TypeData` | 1 | هر دو | یک بستهٔ IP کامل | `core/frame.go:33`؛ `engine/dgpool.go:451`, `779` |
| `TypePing` / `TypePong` | 3 / 4 | هر دو | پرکنندهٔ echo (۰..۹۵ بایت صفر)؛ در پول بی‌اثر | `engine/dgpool.go:797-800`, `912-925` |
| `TypeClose` | 5 | هر دو | `[op u8]`: 0=bye، 1=retire، 2=serve، 3=mute، 4=hear؛ محمولهٔ خالی = bye؛ op ناشناخته نادیده | `engine/dgpool.go:349-360`, `844-871` |
| `TypePoolCtl` | 14 | edge → exit | `[target u16][edgeCeiling u16]`؛ exit قدیمی فقط ۲ بایت اول؛ exit مستقیم target را نادیده می‌گیرد | `core/datagram.go:23`؛ `engine/dgpool.go:1959-1980`, `2056-2061` |
| `TypeLinkStats` | 15 | exit → edge | `[pressed u16][serving u16][exitCeiling u16]`؛ edge قدیمی ۴ بایت اول | `core/datagram.go:24`؛ `engine/dgpool.go:1986-1996`, `2047-2051` |

پیام‌های روی تونل (لایهٔ کاربر، در `dgports.go`):
- **TCP برچسب‌دار** به `:28444`: `[0x01][port u16]` سپس بایت‌های کاربر.
- **probe**: `[0x01][0x00 0x00]` + `encodeInfo(mine)`؛ پاسخ: `encodeInfo(exit.mine)`. قالب info: `[ver=2][n][maxLinks u16][caps][flags][count][ports u16...]` (`engine/peerinfo.go:197-216`)؛ `capPortTags=1`، `flagUDP=1` (edge)، `flagDefault=1` (exit)، `flagCut=2` (فهرست بریده شده).
- **UDP برچسب‌دار**: هر datagram `[0x01][port u16]+payload`؛ پاسخ‌ها بدون برچسب.
- **بدون برچسب**: TCP/UDP خام به `:28443`.

---

## ۸. متن دقیق لاگ‌های مهم

| متن (قالب) | محل | معنی |
|---|---|---|
| `tun %s up: %s peer %s mtu %d (datagram pool, encap %s, %s)` | `cmd/hs2/main.go:851` | TUN باز شد؛ آخرین بخش وضعیت offload است |
| `link pool: coming up at %d links, the size it had before this restart (the autopilot resizes it from there)` | `cmd/hs2/main.go:291` | warm start |
| `dg: carrier %d %s up (now %d)` / `... — spare: pattern needs %d serving` | `engine/dgpool.go:717-719` | حامل آمد (spare فقط در reverse edge) |
| `dg: dropped %d carrier(s) silent for %s+ when carrier %d came up — the other server restarted, or their path died; their flows move to live carriers` | `engine/dgpool.go:714` | حذف zombieها |
| `dg: carrier %d failed (%v) — its flows move to live carriers` | `engine/dgpool.go:730` | شکست I/O (برای بستن عمدی چاپ نمی‌شود) |
| `dg: carrier %d closed by the other server` | `engine/dgpool.go:851` | `closeBye` |
| `dg: carrier %d: the other server hears nothing on it — what goes there is lost: its flows move to live carriers` | `engine/dgpool.go:863` | `closeMute` رسید |
| `dg: carrier %d: the other server hears it again — it takes flows again` | `engine/dgpool.go:867` | `closeHear` رسید |
| `dg: carrier %d has heard nothing from the other server for %.1fs while %d other carrier(s) still do — its own way through is cut: new flows avoid it and %s` | `engine/dgpool.go:1131-1132` | mute محلی |
| `dg: carrier %d hears the other server again — it takes flows again` | `engine/dgpool.go:1112` | خروج از mute |
| `dg: carrier %d heard nothing for %.1fs — closed; a new carrier replaces it` | `engine/dgpool.go:1138` | بستن حامل mute |
| `dg: carrier %d serves again here — the other server stopped renewing its retire notice (its serve notice was lost)` | `engine/dgpool.go:1170` | انقضای retire طرف مقابل |
| `dg: all %d carrier(s) silent for %s+ — dialing a scout carrier to see whether the other server is back` | `engine/dgpool.go:1368` | scout |
| `dg: %s%s` | `engine/dgpool.go:1394` | یادداشت autopilot + `capNote` |
| ` — capped at %d by the Kharej server (its max_links), so at most %d carriers run` | `engine/dgpool.go:1407` | فقط reverse edge، نمایشی |
| `dg: carrier dial failed: %v (%d failed dial(s) since the last line)` | `engine/dgpool.go:1528` | حداکثر هر 30s |
| `dg: carrier %d retired: its flows ended` | `engine/dgpool.go:1597` | بستن retiring خالی |
| `dg: FEC maxed out on %d of %d carriers (mean parity %.0f%% of data) — the pool's loss estimate %.1f%% is past what it can repair (worst active carrier measured %.1f%%)` | `engine/dgpool.go:1775` | FEC در سقف (۲ نمونه) |
| `dg: FEC no longer maxed out on any carrier (mean parity %.0f%% of data)` | `engine/dgpool.go:1781` | خروج (۵ نمونه) |
| `dg: exit target %d carriers (edge asked)` | `engine/dgpool.go:1978` | reverse exit هدف تازه گرفت |
| `dg: +%d more carriers up in the last 10s (latest: ...)` (الگوی عمومی `%s+%d more %s in the last %s (latest: %s)`) | `engine/burstlog.go:76-79` | تا شدن لاگ‌ها؛ گروه‌ها: `carriers up`، `carriers retired`، `carriers closed by the other server`، `mute carrier events`، `carriers failed` (`engine/dgpool.go:605-609`) |
| `dg: tun port %s -> panel %s (every user port without its own target)` | `engine/dgports.go:181` | exit |
| `dg: UDP on tun port %s is not available (%v) — ...` | `engine/dgports.go:179`, `:253` | UDP روی پورت تونل باز نشد |
| `dg: per-port routing is OFF: cannot open tun port %s (%v) — every Iran user port reaches the default panel` | `engine/dgports.go:189` | ۲۸۴۴۴ باز نشد |
| `dg: tun port %s -> per-port targets (%d port(s) with their own target, others -> %s)` | `engine/dgports.go:193` | exit با port_map |
| `dg: user port %s open, forwarded over the tun to the kharej server` | `engine/dgports.go:453` | edge |
| `dg: the kharej server routes each user port to its own target (per-port routing on)` | `engine/dgports.go:620` | yes |
| `dg: the kharej server does not route by user port (an older hs2) — every user port reaches its default panel` | `engine/dgports.go:623` | older |
| `dg: the kharej server's tun answers on port %s but not on %s — filtered there? per-port routing stays off until it answers` | `engine/dgports.go:536` | filtered |
| `ports: Iran user port %d (%s) has no target on this server — ...` | `engine/routes.go:154` | exit، هر دقیقه برای هر پورت/پروتکل |
| `%s: accept failed: %v (retrying; check the open-files limit if this repeats)` | `engine/engine.go:233` | با `what` = `dgtun tagged port` / `user port P` / `forward ADDR` |

---

## ۹. گزینه‌های پیکربندی و متغیرهای محیطی

**فیلدهای پیکربندی** (`cmd/hs2/main.go:36-104`): `carrier: "dgtun"`، `mode`، `reverse`، `addr`، `encap` (udp/icmp/gre/ipip/ipx)، `proto` (فقط ipx)، `bind_local_ip` (`EncapConfig.BindIP`)، `iface`، `local_cidr`، `peer_ip`، `mtu` (پیش‌فرض 1280)، `min_links` (پیش‌فرض 2)، `max_links` (عدد ثابت / 0=auto از سخت‌افزار / غایب=32 / icmp با 0 یا غایب = 8؛ `linkCeiling`، `cmd/hs2/main.go:711-724`)، `per_link` (پیش‌فرض 8)، `forward_ports` و `user_listen_ip` (edge)، `udp`، `expose` و `port_map` (exit)، `shared_key`. `drain_idle_sec` فقط برای پول جریانی است و در dgtun خوانده نمی‌شود (در `runDgTun` استفاده نشده). warm start فقط در edge: `cfg.WarmLinks = warmLinks(...)` (`cmd/hs2/main.go:885-887`، خط `886`).

**متغیرهای محیطی:**

| متغیر | اثر | محل |
|---|---|---|
| `HS2_DG_FQ=0` | خاموش کردن صف عادلانه؛ یک FIFO با دورریز تازه‌وارد و بدون خط سریع | `engine/dgfq.go:66-67` |
| `HS2_TUN_REORDER_MS=n` | زمان نگه‌داشت reorderer؛ 0 = خاموش | `engine/reorder.go:49-56` |
| `HS2_TUN_RCVBUF=n` | rcvbuf پای تونل؛ 0 = autotune هسته | `engine/dgforward.go:75-83` |
| `HS2_TUN_OFFLOAD=0` | خاموش کردن TCP offload روی TUN | `cmd/hs2/main.go:840` |
| `HS2_DG_PAD=0` | خاموش کردن padding اندازهٔ datagram | `udpcarrier/carrier.go:20` |
| `HS2_ICMP_CAMO=1` | استتار زمان‌بندی/شناسهٔ icmp (هر دو سر) | `udpcarrier/carrier.go:35`، `encap/raw_linux.go:29` |
| `HS2_FAIR_SHARE=0` | خاموش کردن قواعد اشتراک گلوگاه در کنترل نرخ | `udpcarrier/rate.go:309` |
| `HS2_RAW_BATCH=0` / `HS2_RAW_TX=0` | ارسال/دریافت تکی؛ خاموش کردن سوکت فقط‌ارسال | `udpcarrier/batch_linux.go:19-26`، `encap/raw_linux.go:400`، `encap/rawtx_linux.go:68` |
| `HS2_ICMP_SUPPRESS=global` | هسته اصلاً به ping پاسخ ندهد (سرور اختصاصی) | `encap/echoguard_linux.go:117` |
| `HS2_PPROF=127.0.0.1:port` | پروفایل Go | `cmd/hs2/pprof.go:21` |

---

## ۱۰. آزمون‌ها: چه چیزی تضمین می‌شود

**`engine/dgpool_test.go`** (حامل جعلی درون‌حافظه‌ای `dgFakeCarrier`):
- `TestDgPoolRoundTrip`: عبور بسته در هر دو جهت در پول مستقیم.
- `TestDgPoolFlowPinning`: یک flow روی یک حامل ثابت؛ ۲۰۰ flow روی ≥۳ از ۴ حامل؛ حامل مرده انتخاب نمی‌شود.
- `TestDgPoolStickyFlows`: افزودن ۶ حامل، retire و un-retire هیچ flow زنده‌ای را جابه‌جا نمی‌کند؛ مرگ حامل یک بار جابه‌جا می‌کند؛ flow مکث‌کرده فراموش می‌شود.
- `TestDgPoolScales`: ۴۰ flow فعال → رشد تا ≥۵ serving (کف ⌈40/8⌉).
- `TestDgReconcile`: رشد تا ۶ (با بودجه)، retire تا ۲، بستن با `drainTick`.
- `TestDgPoolReverse`: عبور دوطرفه در reverse.
- `TestDgDialsInFlightCount`: dialهای در جریان در هدف شمرده می‌شوند (دقیقاً ۱۰ dial برای target=۱۰).
- `TestDgReverseExitLeavesRetiringToEdge`: exit معکوس هرگز خودش retire نمی‌کند.

**`engine/dgcontrol_test.go`** (`dgRig`: دو پول جفت‌شده):
- `TestDgByeClosesTheOtherEnd`، `TestDgRetireMirroredToTheOtherSide` (۲۰۰۰ flow هیچ‌کدام روی retiring)، `TestDgReverseEdgeUnretires` (شامل spare)، `TestDgSilentCarriersDroppedWhenAFreshOneArrives`، `TestDgScoutWhenEveryCarrierIsSilent` (و عدم scout وقتی یکی می‌شنود)، `TestDgShrinkFinishesUnderNonstopDownload` (با `dgRetireForce`=600ms)، `TestDgPeerRetireNoticeExpiresUnlessRenewed`، `TestDgReverseFreshCarrierNotSpareBehindZombies`، `TestDgAddSpareRacesReconcile` (برای `-race`)، `TestDgDirectExitPrunesEndedFlows`.

**`engine/dgmute_test.go`:** `TestDgMuteCarrierAvoidedThenClosed` (0.8s نه، 1.5s mute، 3.2s بسته؛ پرچم `M`؛ `MuteClosed`)، `TestDgMuteCarrierHearsAgain` (حامل با LastRx صفر قضاوت نمی‌شود)، `TestDgMuteNotJudgedWhenEveryCarrierIsSilent`، `TestDgPickHashTiers`، `TestStallGate`، `TestDgMuteToldToTheOtherSide`، `TestDgPeerMuteExpires`، `TestDgLostCarrierLogged`، `TestDgMuteClosedOnce`، `TestDgPoolCtlAvoidsMuteCarrier`.

**`engine/dgfq_test.go`:** `TestFQInteractiveFirst`، `TestFQFairByBytes` (نسبت ۰.۸–۱.۲۵)، `TestFQDropsFromTheFattest`، `TestFQNoOvertake`، `TestFQOffIsTheFIFO`، `TestFQForgetsIdleFlows`، `TestFQSparseFlowStaysAhead` (بازی ۸۰ بایت هر ۱۶ms کنار ۱۰ دانلود)، `TestFQHeavySmoothFlowDoesNotStarve` (دانلود ≥۱۳۵ از ۳۰۰ نوبت)، `TestFQHeavyFlowBackFromPauseHasNoHeadStart`، `TestFQUrgentWaitsForTheDataQueue`، `TestFQFlowQueueStaysBounded` (cap ≤1024 بعد از ۲۰۰هزار push/pop)، `TestDgWriterKeepsAFlowsOrderAcrossLanes`.

**`engine/dgaged_test.go`:** `TestDgAgedDropCountsAsPressure`. **`engine/stage_drop_test.go`:** `TestDgQueueDropsReachTheCarrier` (هر دورریز پر/کهنه به `NoteQueueDrop` می‌رسد).

**`engine/dgdownstats_test.go`:** `TestDgDownStatsFoldsInPressure`، `TestDgDownStatsMaxOfBothDirections`، `TestDgDownStatsWireRoundTrip`، `TestDgDownStatsGuards`، `TestDgFlowSteadyCountsInteractiveUsers`، `TestDgFlowCountsEachConnectionOnce`.

**`engine/dgecho_test.go`:** `TestCarrierEchoShaped` (شامل "ICMP" و " icmp ")، `TestDgEchoReverseEdgeReplyPerRequest`، `TestDgEchoDirectEdgePullsRequests`، `TestDgPoolICMPShapedBothModes`، `TestDgEchoHeavySideNoFiller`.

**`engine/dgpeermax_test.go`:** `TestDgPeerMaxFromLinkStats`، `TestDgPeerMaxFromPoolCtl`، `TestDgPeerMaxPayloadLayout`، `TestDgPeerMaxDirectBothServers`، `TestDgPeerMaxReverseBothServers`. **`engine/dgdiag_test.go`:** `TestSendDiag`.

**`engine/dgforward_test.go`:** `TestDgTunPortIsPrivate`، `TestDgExitForwarderSurvivesPanelOnWildcard`، `TestDgForwarderUserPortsLiveOnlyOnEdge`، `TestDgExitWithoutPanelOpensNothing`.

**`engine/dgports_test.go`:** `TestDgPortsRouteEachPort`، `TestDgPortsOlderExit`، `TestDgPortsOlderEdge`، `TestDgPortsForeignListenerOnTagPort`، `TestDgPortsNoDefault`، `TestDgPortsTagPortFiltered`، `TestDgPortsTunDownStaysUnknown`، `TestDgPortsSilentListenerOnTagPort`، `TestDgPortsRefusedUDPFlowKeepsState`، `TestDgPortsUDPFlowFollowsState`، `TestDgPortsTunUpMidProbe`، `TestDgPortsExitUDPFlowReuse`، `TestDgPortsEmptySides`، `TestDgPortsFlapIsNotFiltered`، `TestDgPortsTCPFollowsExitTableAtOnce`، `TestDgPortsStartWaitsForTun`.

**دیگر:** `engine/tunbatch_test.go`: `TestDgReadLoopBatchesTunWrites`، `TestTunBatchCountsWhatWentIn`؛ `engine/reorder_test.go`: ۱۲ آزمون reorderer؛ `engine/shutdown_test.go`: `TestRunClosesListenerBeforeReturning` (listener پیش از بازگشت Run بسته شده — قاعدهٔ nft icmp)؛ `engine/exitstats_test.go`: `TestDgStatsCountedWithPeak`؛ `engine/peerinfo_test.go:277-287` (capNote برای dgtun).

**`engine/carrier_udp_test.go`** (حامل‌های `auto`/`udp`، نه dgtun): `TestAutoSelectsUDPWhenGood`، `TestProbeSeesBlockedUDP`، `TestAutoFallsBackToTCP`.

آنچه آزمون **ندارد** (بر پایهٔ جست‌وجو؛ نامطمئن نسبت به آزمون‌های lab): رفتار autopilot با حامل‌های واقعی زیر فشار، چرخهٔ close/redial در reverse وقتی `min_links` exit بالاتر از هدف edge است، و جای‌گذاری در حضور حامل فشرده.

---

## ۱۱. «از قبل وجود دارد» (برای جلوگیری از دوباره‌کاری)

1. پول N حامل datagram زیر یک TUN با autopilot مشترک با mtcp (direct و reverse).
2. جای‌گذاری per-flow با rendezvous hashing + **flowlet چسبنده** (۳۰۰ms) + طبقه‌بندی serving/retiring/avoid.
3. صف ارسال هر حامل **DRR عادلانه** با «flow تُنُک اول» (۲۵۶kbit/s، burst ۸KB) و **خط سریع pacer** با حفظ ترتیب از طریق `LaneMark/LaneDrained`؛ دورریز از سر چاق‌ترین flow.
4. محدودیت sojourn (۵۰ms) با شمارش دورریز کهنه به‌عنوان **فشار** و اطلاع به مدل نرخ حامل (`NoteQueueDrop`).
5. بافر مرتب‌سازی TCP به‌ازای هر حامل (۱۵ms) و نوشتن دسته‌ای TUN با GRO؛ TCP offload روی TUN؛ خواندن دسته‌ای از حامل (۶۴).
6. تشخیص **mute** در ۱s با اطلاع دوطرفه (`closeMute/closeHear`)، بستن در ۳s، انقضای ۴s، `stallGate`.
7. **zombie** و **scout** برای ری‌استارت/قطع کامل.
8. `closeBye` دوباره، **آینه‌کردن retire**، یادآوری retire و انقضای آن، `dgRetireForce` برای پایان کوچک‌شدن زیر دانلود مداوم.
9. spare در reverse edge با `bornSpareGrace` و `dgRetireHold`؛ un-retire پیش از dial.
10. کانال برگشت **فشار دانلود** (`TypeLinkStats`) و max(بالا، پایین).
11. شمارش flow «steady» (کاربران تعاملی) و حذف شمارش دوبرابر دو جهت.
12. dialها از **gate سراسری**؛ dialهای در جریان در هدف شمرده می‌شوند؛ epoch برای رها کردن dialهای بی‌فایده.
13. **warm start** از هدف قبل از ری‌استارت.
14. نمایش سقف طرف مقابل در قاب‌های کنترلی (بدون نوع قاب جدید).
15. **governor** پول برای policer (udpcarrier) و سهم عادلانه.
16. شکل‌دهی **echo ۱:۱** روی icmp با پرکنندهٔ کران‌دار.
17. وضعیت مفصل: خط `carriers` با پرچم‌های `P/S/C/M`، شمارش بسته در هر مرحله، دورریز به تفکیک علت، FEC، reorder، policer، مرحلهٔ ارسال (`sending:`).
18. لاگ‌های تاشونده (`burstLog`) برای صدها حامل.
19. forward پورت با پورت خصوصی ۲۸۴۴۳، rcvbuf ثابت ۴MB، **مسیریابی هر پورت** با probe روی ۲۸۴۴۴ و سازگاری با همهٔ ترکیب نسخه‌ها.
20. سقف ۸ برای icmp.

---

## ۱۲. مقایسهٔ دقیق dgtun و l3mtcp

> زمینه: در l3mtcp اتصال TCP کاربر روی edge پایان می‌یابد و بایت‌هایش روی یک **smux stream** از یک لینک TLS-روی-TCP می‌رود (`engine/stream.go:20-31`)؛ TUN (`hs0`) فقط **کانال جانبی** برای ترافیک غیر-TCP (ping، UDP به 10.77.x) است که روی یک stream خام هر لینک حمل می‌شود (`engine/stream_iran.go:90-94`, `419-438`). در dgtun، TUN **مسیر اصلی** است.

| جنبه | dgtun | l3mtcp |
|---|---|---|
| حمل TCP کاربر | پای میانی = TCP مستقل هر کاربر داخل datagramها (forwarder↔forwarder) | smux stream روی لینک TLS/TCP مشترک (پنجرهٔ smux، HOL بین کاربران یک لینک) |
| TUN | مسیر اصلی؛ offload؛ MTU ۱۲۸۰ | کانال جانبی؛ `tun.Open` بدون offload؛ MTU ۱۳۸۰ (`cmd/hs2/main.go:458-466`) |
| واحد جای‌گذاری | بسته/flow ۵-تایی، rendezvous + sticky | اتصال کاربر با `Pick` **آگاه از بار** (بدون فشار اول، کمترین flowing+picks، کمترین users، سقف burst، شکستن تساوی تصادفی؛ `engine/linkmanager.go:1464-1612`)؛ کانال جانبی TUN: rendezvous **ساده بدون sticky** (`engine/l3_link.go:308-323`) |
| آگاهی جای‌گذاری از فشار | **ندارد** (hash یکنواخت) | دارد (`pickKey.pressed`) |
| صف ارسال | fq/DRR + خط سریع + دورریز چاق‌ترین؛ ۲۵۶ بسته؛ ۵۰ms | streamها: فشار برگشتی smux؛ کانال TUN: FIFO کانالی ۲۵۶، دورریز تازه‌وارد، ۶۰ms، تجمیع تا ۱۶KB در یک نوشتن TLS (`engine/l3_link.go:49-62`, `202-243`) |
| مرتب‌سازی | reorderer ۱۵ms | لازم نیست (TCP مرتب) |
| نوشتن TUN | `tunBatch` + GRO | `dev.Write` تک‌به‌تک (`engine/l3_link.go:361-377`) |
| اتلاف | FEC تطبیقی + governor در حامل | بازارسال TCP هسته؛ تشخیص **degraded** (بازارسال >۱۲٪ در ۳ نمونه) و drain/جایگزینی (`engine/health.go:29-34`, `52-76`) |
| جایگزینی حامل/لینک پراتلاف | **ندارد** | دارد (heal، make-before-break) |
| زنده‌بودن | feedback ۱۰۰ms → mute ۱s، silent ۳s، deadAfter ۱۵s، scout، zombie | keepalive smux، `suspectAfter` ۱۲s (`engine/linkmanager.go:317`)، کانال TUN: deadAfter ۸s/۳۰s، `l3SessionSilent` ۱۲s، keepalive ۲s یا ۱۰s آرام (`engine/l3_link.go:63-87`)، wedge/stuck |
| اطلاع قطع یک‌طرفه به طرف مقابل | `closeMute/closeHear` | نامطمئن (در خواندن من دیده نشد) |
| سیگنال فشار | دورریز صف یا کهنه در همین تیک + گرم + بدون سقف governor | writer مسدود ≥۵۰٪ زمان و ≥۱۶KB در تیک و سهم rwnd <۵۰٪، چسبنده ۲ از ۳ نمونه (`engine/linkmanager.go:65-72`, `1612`) |
| فشار دانلود | `TypeLinkStats` تجمیعی (تعداد) | `kindStats` رکورد هر لینک |
| کانال کنترل RTT/retrans | ندارد در لایهٔ پول (داخل حامل هست) | `kindCtrl` هر ۳s (`engine/health.go:92-95`) |
| retire/drain | flowlet ۳۰۰ms؛ اجبار ۳۰s؛ بدون بستن اتصال کاربر | تا پایان اتصال‌ها؛ `drainIdle` ۳۱۰s؛ `retireForce` ۲۰ دقیقه (`engine/linkmanager.go:82-91`) |
| محافظ churn در reverse | فقط `dgRetireHold` و `bornSpareGrace` | `churnTrips/churnWindow/churnHold` (`engine/linkmanager.go:95-104`) |
| refill hold | ندارد (جای‌گذاری per-packet) | دارد (`refill.go`، `engine/linkmanager.go:201-203`) |
| `growable` در نمونهٔ autopilot | همیشه `true` (`engine/dgpool.go:600`) | در reverse بی‌کنترل پول `false` می‌شود (`engine/linkmanager.go:1995`) |
| وضعیت | `Pressed/Saturated/CapMbit/NextProbeS/HeldBy` **پر نمی‌شوند** (`engine/dgpool.go:1677-1683`) | پر می‌شوند (`engine/linkmanager.go:2297-2372`) |
| لاگ دورریز TUN | فقط شمارنده | هر ۳۰s: `l3: dropped %d packets in 30s on the tun side channel ...` (`engine/l3_link.go:389`) |
| شمارش ترافیک TUN در اندازه‌گیری | flowهای TUN همان flowهای اندازه‌گیری‌اند | stream خام TUN «کاربر» شمرده نمی‌شود (`engine/linkmanager.go:211-215`) ولی بایت‌هایش در `meteredConn` لینک شمرده می‌شود (`engine/health.go:169-195`) |
| مسیریابی هر پورت | پورت ۲۸۴۴۴ + probe | `kindTCPPort/kindUDPPort` + `kindInfo` (`engine/stream.go:33-46`) |
| fallback به TCP | ندارد (فقط encapهای datagram) | خودش TCP است |

**ایده‌هایی که در dgtun هست و می‌تواند برای l3mtcp (یا کانال TUN آن) مطرح باشد:** صف عادلانهٔ per-flow و خط سریع؛ flowlet چسبنده (کانال TUN l3 چسبندگی ندارد و با افزودن لینک ~1/n flowها جابه‌جا می‌شوند)؛ نوشتن دسته‌ای TUN و offload؛ اعلام صریح mute/hear به طرف مقابل؛ zombie-on-fresh-link.
**ایده‌هایی که در l3mtcp هست و dgtun ندارد:** جای‌گذاری آگاه از فشار/بار؛ تشخیص و جایگزینی لینک پراتلاف/کند (degrade/heal)؛ فشار با حداقل بایت و چسبندگی ۲ از ۳؛ محافظ churn در reverse؛ `growable=false`؛ refill hold؛ لاگ دوره‌ای دورریز؛ فیلدهای `Pressed/CapMbit/...` در وضعیت؛ outage scout مستقل با سقف تلاش اول (`engine/regress300_test.go:124`, `193`).

---

## ۱۳. ایده‌هایی که امتحان و رد شده‌اند (طبق کد یا مستندات)

1. **یک FIFO برای هر حامل** → ping زیر بار ۶۶/۹۴ms (p50/p99)؛ با fq ۱۲/۲۰ms؛ FIFO فقط با `HS2_DG_FQ=0` (`engine/dgfq.go:13-17`؛ CHANGELOG فاز V4).
2. **رفتار fq_codel برای flow تازه‌خالی‌شده** (ماندن در فهرست old تا نوبتش) → بستهٔ بعدی بازی یک دور کامل پشت دانلودها می‌ماند؛ جایگزین با سطل توکن تُنُکی (`engine/dgfq.go:23-29`).
3. **محافظ ترتیب ثابت ۱۰۰ms** برای خط سریع → در نرخ کف یا زیر سقف policer سبقت رخ می‌داد؛ جایگزین با شمارش صف داده؛ ۱۰۰ms فقط fallback (`engine/dgfq.go:30-37`, `50-54`؛ CHANGELOG «Review fixes» فاز V).
4. **جست‌وجوی چاق‌ترین flow در همهٔ flowهای ۲s اخیر** → فقط flowهای صف‌دار (CHANGELOG همان بخش؛ `engine/dgfq.go:200-207`).
5. **rendezvous بدون چسبندگی** → ~1/n flowها با هر تغییر جابه‌جا می‌شدند (میدان: ۴۰٪ بایت بازارسال روی مسیر بی‌اتلاف) (`engine/dgpool.go:549-554`؛ `engine/dgpool_test.go:184-188`).
6. **نگه داشتن flowهای چسبیده روی retiring تا ابد** → کوچک‌شدن هرگز تمام نمی‌شد؛ `dgRetireForce` (`engine/dgpool.go:342-347`).
7. **`closeServe` تک‌ارسالی بدون انقضا** → حامل برای همیشه retiring در طرف مقابل (`engine/dgpool.go:274-279`).
8. **فقط دورریز صف پر به‌عنوان فشار** → پول زیر همان باری که لازمش داشت کوچک می‌شد («0 of 8 at their limit») (`engine/dgpool.go:434-439`؛ `engine/dgaged_test.go:8-14`).
9. **شمارش flowing فقط با EWMA** → ۲۴۰۰ کاربر تعاملی ۷۳–۸۸۷ شمرده شدند و پول ۱۰۰s روی ۸ ماند (`engine/dgpool.go:1269-1275`).
10. **جمع دو جهت** → دقیقاً دو برابر (۴۸۳۲ برای ۲۴۱۶) (`engine/dgpool.go:1318-1321`).
11. **ماندن حامل‌های ساکت تا ۱۵s** و **ماندن حامل mute با flowهایش** → zombie/scout/mute (`engine/dgpool.go:57-74`).
12. **اسکن flow در هر مقایسهٔ sort** → ۱۲ms قفل پول در ۳۰۰ حامل (`engine/dgpool.go:1638-1642`).
13. **`dgReorderHold` ۲۵ و ۴۰ms** → روی اتلاف انفجاری هزینه بیشتر از سود (۴۰ms حدود ۱۰٪ گذردهی کمتر) (`engine/reorder.go:39-48`).
14. **rcvbuf ۸ و ۱۶MB** → گذردهی تغییری نکرد؛ ۴MB ماند (`engine/dgforward.go:65-74`).
15. **گوش دادن exit روی پورت کاربر P** → `EADDRINUSE` در برابر panel روی wildcard و crash-loop؛ پورت خصوصی ۲۸۴۴۳ (`engine/dgforward.go:33-38`).
16. **mux قابل‌اطمینان روی حامل** → دام TCP-in-TCP؛ رد شد (`engine/dgforward.go:16-22`).
17. **مقایسهٔ دقیق «icmp»** → `EqualFold`+`TrimSpace` (`engine/dgpool.go:889-892`).
18. **شمارش پرکننده‌های دریافتی در echo** → حلقهٔ بی‌پایان پرکننده؛ رد شد (`engine/dgpool.go:769-774`).
19. **برچسب UDP همیشه** → فقط وقتی exit جدول دارد (`engine/dgports.go:29-32`)؛ **نگه داشتن کپی جدول exit در edge برای TCP** → TCP همیشه برچسب (CHANGELOG I2 review)؛ **خطای UDP خاموش‌کنندهٔ کل مسیریابی** → فقط poke (`engine/dgports.go:806-811`)؛ **پاسخ دیررس = older** → unknown (`engine/dgports.go:556-564`).
20. **بیش از ۸ حامل روی icmp** → پهنای باند اضافه نمی‌دهد (یک مسیر، یک policer) فقط الگوی غیرعادی (`cmd/hs2/main.go:671-677`).
21. **یک سوکت برای هر حامل روی encap خام** → هر بسته به همهٔ سوکت‌ها کپی می‌شد؛ سوکت مشترک (CHANGELOG Q5). **سقف موقت ۶۴/۱۲۸** پس از آزمون بار برداشته شد (CHANGELOG Q1).
22. **صف ارسال عمیق‌تر زیر اشباع پردازنده** «امتحان و رها شد: عددی را تکان نداد» (CHANGELOG فاز Y، حوالی خط ۱۵۵۳ از `CHANGELOG.md` ریشه). نامطمئن: متن مشخص نمی‌کند منظور صف پول (`dgQueueLen`) است یا صف pacer.
23. **استتار icmp با سکوت کامل حامل بیکار** → flap با آشکارساز mute ۱s؛ CA1b ضربان ~۰.۷s گذاشت (`udpcarrier/carrier.go:36-40`, `562-571`). **CA3** (نگه داشتن بسته‌ها زیر آستانهٔ اندازه) و **CA4** (هم‌بستگی کامل درخواست/پاسخ) ارزیابی و ساخته نشدند (CHANGELOG فاز CA).
24. **پول ترکیبی icmp+udp** روی مسیر آزمون میدانی سودی نداشت (حامل‌های udp بالا آمدند و ۱۰۰٪ اتلاف داشتند) — مشاهدهٔ میدانی، نه تصمیم طراحی (CHANGELOG فاز V).

---

## ۱۴. محدودیت‌های شناخته‌شده و مشاهده‌ها

> برچسب «مشاهده» = نقطه‌ضعف احتمالی که از خواندن کد دیدم؛ هیچ پیشنهاد تغییری داده نمی‌شود.

- **مشاهده ۱ — جای‌گذاری از فشار بی‌خبر است.** `pickHash` فقط rendezvous و طبقهٔ retire/avoid را می‌بیند (`engine/dgpool.go:1015-1043`)؛ حامل فشرده همچنان ~1/n flowهای جدید را می‌گیرد و flowهای چسبیده تا مکث ۳۰۰ms جابه‌جا نمی‌شوند. در mtcp، `Pick` حامل بدون فشار و لینک تازه را ترجیح می‌دهد.
- **مشاهده ۲ — سیگنال probe رشد ممکن است رقیق شود (استنتاج).** حامل‌های تازهٔ یک probe فقط سهم hash از flowهای **جدید/مکث‌کرده** را می‌گیرند (نه همهٔ flowهای جدید مثل mtcp که لینک تازه کلید صفر دارد)؛ این ممکن است آزمون «جمع‌پذیری» autopilot را ضعیف کند. نامطمئن: اثر عملی اندازه‌گیری نشده.
- **مشاهده ۳ — فشار تک‌رخدادی.** یک دورریز در یک تیک (`now - droppedAt <= dt`) حامل را pressed می‌کند؛ حداقل بایت (مثل `pressMinBytes`) یا چسبندگی ۲ از ۳ ندارد (`engine/dgpool.go:1198`). هموارسازی فقط در خود autopilot (کمبود در ۳ از ۵ تیک) است.
- **مشاهده ۴ — flow ناپاسخگو.** یک flow UDP سریع‌تر از حاملش (مثلاً VPN روی UDP داخل تونل) پیوسته دورریز «چاق‌ترین» می‌سازد و حاملش را pressed نگه می‌دارد، ولی چون چسبیده است رشد پول کمکش نمی‌کند.
- **مشاهده ۵ — نبود جایگزینی حامل پراتلاف/کند.** حامل زنده با اتلاف یا نرخ کم جایگزین نمی‌شود (فقط mute/silent/failed/bye)؛ در mtcp `degraded` هست. throttle per-5-tuple روی پورت UDP یک حامل خاص فقط از طریق رشد پول جبران می‌شود.
- **مشاهده ۶ — احتمال churn در reverse.** اگر `min_links` در exit از هدف edge بیشتر باشد: exit بالای هدف edge dial می‌کند (`onPoolCtl` clamp به min خودش، `engine/dgpool.go:1970-1972`)، edge اضافه‌ها را retire و پس از ۶s می‌بندد، exit دوباره dial می‌کند، حامل‌های تازه spare می‌شوند و ۳۰s بعد بسته می‌شوند → چرخهٔ تقریباً ۳۰ثانیه‌ای close/redial. پول جریانی محافظ churn دارد (`engine/linkmanager.go:95-104`)؛ dgPool ندارد. (استنتاج از کد؛ آزمونی برایش نیست.)
- **مشاهده ۷ — `growable` همیشه true.** در reverse edge، autopilot از سقف exit خبر ندارد؛ `capNote` فقط در لاگ می‌گوید (`engine/dgpool.go:1399-1410`).
- **مشاهده ۸ — حاشیهٔ باریک mute زیر `HS2_ICMP_CAMO=1`.** ضربان بیکار ۵۶۰–۸۴۰ms است و آستانهٔ mute ۱s با تیک ۲۵۰ms؛ گم‌شدن **یک** گزارش feedback روی حامل بیکار می‌تواند آن را mute کند (در حالت عادی ده گزارش پیاپی لازم است). اثر اصلی: لاگ و پیام‌های `closeMute/closeHear` (`udpcarrier/carrier.go:562-571`, `engine/dgpool.go:74`).
- **مشاهده ۹ — قفل‌ها روی مسیر داده.** `sampleHealth` کل حلقهٔ حامل‌ها (شامل `flowStats` روی نقشهٔ flow هر حامل) را زیر `p.mu.Lock()` انجام می‌دهد (`engine/dgpool.go:1160-1210`)؛ `pickHash` برای flow جدید یا flow غیرچسبیده `p.mu.RLock()` می‌خواهد و `pumpTun` تک‌goroutine است. همچنین `pruneSticky` کل نقشهٔ sticky را زیر `stickyMu` می‌پیماید که `pick` هر بسته می‌گیرد (`engine/dgpool.go:979`, `999-1007`). اندازهٔ مکث در مقیاس بالا نامطمئن.
- **مشاهده ۱۰ — جابه‌جایی بین حامل‌ها مرتب‌سازی ندارد.** reorderer به‌ازای هر حامل است؛ وقتی flow (پس از مکث، mute، مرگ یا `dgRetireForce`) حامل عوض کند، بسته‌های در راه روی حامل قبلی ممکن است بعد از بسته‌های حامل جدید برسند و پنهان نمی‌شوند. README می‌گوید «a pool resize never moves a live flow» (`README.md:471`)، ولی کد پس از ۳۰s retire و هنگام mute جابه‌جا می‌کند.
- **مشاهده ۱۱ — IPv6 و غیر TCP/UDP.** `flowHash` برای IPv6 فقط دو آدرس را hash می‌کند (`engine/l3_link.go:410-411`): همهٔ اتصال‌های IPv6 بین دو میزبان یک flow (یک حامل، یک صف fq) هستند. ترافیک dgtun forwarder روی IPv4 تونل است، پس این فقط برای ترافیک مسیریابی‌شدهٔ IPv6 مهم است.
- **مشاهده ۱۲ — بالادست/پایین‌دست یک اتصال روی حامل‌های متفاوت.** هر سر با seed خودش hash می‌کند؛ پس داده و ACK یک اتصال معمولاً روی دو حامل‌اند (طراحی، `engine/dgpool.go:223-226`). نامطمئن: اثر روی RTT/ACK clocking.
- **مشاهده ۱۳ — قطع کامل در direct edge.** وقتی هیچ حاملی نیست، scout فعال نمی‌شود (`n==0`) و `reconcile` هر تیک تا بودجه (≥۴) dial صف می‌کند؛ هر dial ناموفق تا ~۱۰s handshake (+۶s confirm) یک جای gate را می‌گیرد (`udpcarrier/dial.go:126`, `190-198`). backoff جداگانه‌ای جز epoch و لاگ ۳۰s نیست.
- **مشاهده ۱۴ — ابهام شمارش serving در exit مستقیم.** exit مستقیم هرگز `l.retiring` را تنظیم نمی‌کند؛ حامل‌هایی که edge retire کرده در `publishDownStats` جزو serving و pressed exit شمرده می‌شوند (`engine/dgpool.go:2033-2042`). `dnServing` در edge فقط ذخیره می‌شود و جایی مصرف نمی‌شود (`engine/dgpool.go:532`, `1991`).
- **مشاهده ۱۵ — وضعیت ناقص‌تر از mtcp.** `Pressed`، `Saturated`، `CapMbit`، `NextProbeS`، `HeldBy` در snapshot dgtun پر نمی‌شوند (`engine/dgpool.go:1677-1683`)؛ عدد فشرده فقط در متن `Reason` autopilot می‌آید.
- **مشاهده ۱۶ — پیام‌های کنترلی بدون بازارسال.** `closeRetire` تمدید می‌شود و `closeBye/closeMute` دو بار ارسال می‌شوند، ولی `closeServe` و `closeHear` یک بار (با انقضا جبران شده) و `TypePoolCtl/TypeLinkStats` فقط با تکرار دوره‌ای.
- **محدودیت‌های پیش‌موجود که CHANGELOG ذکر کرده** (لایهٔ حامل/نرخ، فاز W): زیر policer پایدار بدون رخداد اتلاف، پول ~۲.۵ برابر عبوری می‌فرستد و parity به سقف می‌رسد؛ flowهای ریز زیاد پشت گلوگاه مشترک همه «تُنُک» شمرده می‌شوند و fq نمی‌تواند echo UDP را جدا کند؛ ۱۶ حامل روی گلوگاه ۸Mbit/s به نرخ کف فرو می‌ریزند؛ ۸ حامل روی مسیر ۱۶–۲۴Mbit/s و ۸۰ms ممکن است روی بافر پر قفل شوند (اصلاح در «loss compensation» به تغییری جدا موکول شده).

---

## ۱۵. ارجاع به زیرسیستم‌های دیگر

**dgtun صدا می‌زند:**
- `engine/autopilot.go`: `newAutopilot`، `decide`، `apSample/apLink/apDecision`، `ceilDiv`، `fmtDur`، `mbitps` — منطق اندازه‌گیری کاملاً مشترک با mtcp.
- `udpcarrier`: `Conn` (`SendFrame`، `SendUrgent`، `LaneMark/LaneDrained`، `NoteQueueDrop`، `ReadFrame/TryReadFrame`، `LastRx`، `Warm`، `Stats`، `Encap`، `AttachGovernor`)، `Governor` (`Capped/Confirmed/CapBytes/Share/BusyQueue/Last/Run`)، `DialCfg/ListenCfg` (`engine/dgcarrier.go`).
- `engine/reorder.go` (`newReorderer`, `Push`, `Stats`, `Close`) و `engine/tunbatch.go` — فقط dgtun از این دو استفاده می‌کند (جست‌وجوی `newReorderer`/`newTunBatch` فقط در `dgpool.go`).
- `engine/l3_link.go`: `tunWriter`، `qpkt`، `flowHash`، `mix32` (مشترک با کانال جانبی l3).
- `engine/health.go`: `healthTick`، `flowRecent`، `flowingRate`، `flowSteadyRate`؛ `engine/mtcp_link.go:175`: `flowAlpha`؛ `engine/linkmanager.go`: `warmSize`، `bornSpareGrace`، `closeJitter`، `dialFailLogEvery`، `b2u`، `PoolStats`؛ `engine/dialgate.go`: `linkGate`؛ `engine/burstlog.go`؛ `engine/exitstats.go`: `peakTicks`، `peakOf`؛ `engine/engine.go`: `sleepCtx`، `acceptBackoff`.
- dgports: `engine/peerinfo.go` (`peerInfo`، `encodeInfo`، `readInfo`، `capPortTags`، `infoTimeout`)، `engine/routes.go` (`RouteTable`، `noRouteLog`، `exitRoutes`، `PeerRoutes`)، `engine/listen.go` (`listenReuseRcvBuf`)، `engine/stream.go` (`relay`، `kindTimeout`)، `engine/rcvbuf_*.go` (`setRcvBuf`).
- `core`: انواع قاب (`core/frame.go:32-38`, `core/datagram.go:13-25`).
- `tun`: `OpenWith` با offload، `Read` تقسیم‌کنندهٔ GSO، `WriteBatch`.

**dgtun از کجا صدا زده می‌شود:**
- `cmd/hs2/main.go:420-421` → `runDgTun` (`:829-903`): `StartDgPorts`، `NewDgDialer/NewDgListener`، `RunDgEdge/RunDgExit`، `startStatusWriter` (افزودن `Routes` و آمار offload به `PoolStats`، `:875-883`)، `warmLinks`، `linkEnvelope/linkCeiling`.
- `engine/carrier_udp.go` از `cmd/hs2/main.go:795-819` (`runUDP`) برای حامل‌های `udp`/`auto` تک‌جلسه‌ای صدا زده می‌شود، نه برای dgtun.
- آزمایشگاه: `lab/dgtun.sh` (دو namespace با netem، همهٔ encapها)، `lab/cpuquota.sh` (فرستندهٔ کم‌پردازنده)، `lab/dglab/main.go` (بار حامل خام).
