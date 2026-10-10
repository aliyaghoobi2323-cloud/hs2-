# ابزارهای عملیاتی و تنظیم سیستم

> دامنه: `cmd/hs2/status.go`، `cmd/hs2/doctor.go` (به‌همراه `doctor_cert.go` و `hostcpu.go` که doctor و status مستقیم به آن‌ها تکیه دارند)، `tune/tune.go` و `tune/tune_test.go`، و `cmd/hs2/linkpool_test.go`. بخش‌های لازم از `cmd/hs2/main.go` (اجرای tune، سقف لینک، فایل warm) هم آمده است.
>
> قرارداد ارجاع: مسیرها نسبت به `hs2-src/` هستند (مثلاً `tune/tune.go:198`)، مگر `install.sh`، `README.md` و `CHANGELOG.md` که در ریشهٔ مخزن `/home/user/hs2-` هستند. شاخه برابر ثبت `0812bc9` است. همهٔ آزمون‌های مرتبط (`go test ./tune/` و آزمون‌های نام‌برده در بخش ۱۰ از `./cmd/hs2/`) در این محیط با `go1.27.0` اجرا شدند و **همه سبز** بودند.

---

## ۱. نقش و جایگاه در کل سیستم

این زیرسیستم سه کار جدا انجام می‌دهد که به هم گره خورده‌اند:

1. **تنظیم هستهٔ لینوکس (بستهٔ `tune`)**: در هر `hs2 run` از روی RAM و تعداد هسته‌ها یک «برنامه» (Plan) از sysctlها، الگوریتم کنترل ازدحام (پیش‌فرض `bbr`) و qdisc (پیش‌فرض `fq_codel`) می‌سازد و (اگر ریشه باشد و `HS2_NO_TUNE` خالی باشد) اعمال می‌کند (`cmd/hs2/main.go:351-356`). «تنها منبع حقیقت» تنظیم است؛ نصاب دیگر فایل `/etc/sysctl.d/99-hs2.conf` نمی‌نویسد و نسخهٔ قدیمی‌اش را پاک می‌کند (`install.sh:294-307`).
2. **سقف خودکار پول لینک** (`tune.RecommendedMaxLinks`): سقف لینک‌های موازی mtcp/l3mtcp/dgtun را از RAM و هسته‌ها حساب می‌کند (`tune/tune.go:177-251`) و `cmd/hs2/main.go:711-724` آن را با `max_links` پیکربندی ترکیب می‌کند. این عدد سقف «اتوپایلوت» است، نه هدف آن.
3. **دیدپذیری و تشخیص**:
   - **فایل وضعیت زنده** (`/run/hs2/<مسیر-پیکربندی>.status.json`) که هر ۲ ثانیه اتمی نوشته می‌شود (`cmd/hs2/status.go:29,250-349`) و `hs2 status`، `hs2 doctor`، `hs2 ports` و منوی نصاب از آن می‌خوانند.
   - **`hs2 status`**: داشبورد یک‌صفحه‌ای از روی همان فایل (`cmd/hs2/status.go:422-536`).
   - **`hs2 doctor`**: بررسی سلامت فقط‌خواندنی (`cmd/hs2/doctor.go:29-85`).
   - **`hs2 tune`** و **`hs2 recommend-links`**: نمایش برنامهٔ تنظیم و سقف پیشنهادی (`cmd/hs2/main.go:572-624`).

**نکتهٔ مهم معماری (کمتر دیده می‌شود):** نویسندهٔ فایل وضعیت فقط «نمایش» نیست؛ **دو سیگنال کنترلی مسیر داده را هم تولید می‌کند**:
- `engine.SetTCPMemPressure(...)` در هر تیک (`cmd/hs2/status.go:854`) — نگهبان «خوانندهٔ گیرکرده» (`engine/wedge.go:158`) و قواعد سلامت لینک (`engine/linkmanager.go:1671-1677`) از آن استفاده می‌کنند و لبه آن را از طریق پرچم `statsFlagMemPressure` در رکوردهای آمار از سمت مقابل هم می‌گیرد (`engine/stats.go:51,239`).
- `udpcarrier.SetHostSaturated(...)` در هر تیک (`cmd/hs2/status.go:330`) — اعتبار جبرانی `pacerSatCredit` = 50ms در pacer حامل‌های datagram (`udpcarrier/pacer.go:120-130,307`).

برای **تونل اصلی l3mtcp** (TUN + mtcp): پول لینک دارد (`hasLinkPool`، `cmd/hs2/main.go:758-764`)، پورت کاربر دارد (`hasUserPorts`، `cmd/hs2/ports.go:36-42`)، حامل TCP/TLS است (پس doctor نقطهٔ پایانی و گواهی را بررسی می‌کند، `cmd/hs2/doctor.go:144,172`)، و رابط `hs0` دارد (پس `checkTun` اجرا می‌شود). برچسب انتقال آن در وضعیت «`tun (L3 over mtcp)`» است (`cmd/hs2/status.go:807-808`).

---

## ۲. اجزای اصلی

### ۲.۱ بستهٔ `tune` (`tune/tune.go`)

| جزء | مکان | توضیح |
|---|---|---|
| `ModeAuto`/`ModeManual`/`ModeOff` | `tune/tune.go:33-37` | سه حالت تنظیم: خودکار، خودکار + جایگزینی دستی، خاموش |
| `type Config` | `tune/tune.go:41-52` | بخش `"tuning"` پیکربندی: `mode`، `congestion`، `qdisc`، `rmem_max`، `wmem_max`، `netdev_backlog`، `somaxconn` |
| `type Plan` | `tune/tune.go:55-64` | خروجی تصمیم: Mode، Profile، RAMMB، CPUs، Congestion، Qdisc، Sysctls مرتب، Notes |
| `Detect()` | `tune/tune.go:74-80` | RAM (MemTotal) و هسته‌ها؛ اگر محدودیت حافظهٔ cgroup کمتر باشد همان؛ هسته = `min(NumCPU, GOMAXPROCS)` |
| `cgroupMemLimitMB` | `tune/tune.go:85-137` | v2: کمینهٔ `memory.max` در کل مسیر تا ریشه؛ v1: `hierarchical_memory_limit` از `memory.stat` و `memory.limit_in_bytes`؛ مقدار ≥ `1<<50` یعنی «بی‌حد» |
| `detectRAMMB` | `tune/tune.go:139-155` | `MemTotal` از `/proc/meminfo` تقسیم بر ۱۰۲۴ |
| `profileFor` / `ProfileFor` | `tune/tune.go:159-175` | طبقه‌بندی low/medium/high |
| ثابت‌های سقف لینک | `tune/tune.go:198-213` | ۳۲/۴۸/۶۴، `LinkWorstCaseMiB=12`، `LinkRAMPerLinkMB=48`، `MaxLinksCap=300`، `maxLinksFewCores=128` |
| `maxLinksRule` / `RecommendedMaxLinks` / `MaxLinksReason` | `tune/tune.go:229-270` | قاعدهٔ سقف و متن دلیل آن |
| `profileValues` | `tune/tune.go:280-289` | بافر/backlog/somaxconn هر پروفایل |
| `Build` | `tune/tune.go:294-372` | تابع خالص ساخت Plan (دسترس‌پذیری cc/qdisc تزریق می‌شود) |
| `pick` | `tune/tune.go:376-393` | انتخاب با زنجیرهٔ جایگزین و یادداشت |
| `(*Plan).Apply` | `tune/tune.go:398-448` | modprobe، نوشتن sysctlها، جایگزینی qdisc، لاگ |
| `Summary` / `Report` | `tune/tune.go:451-481` | یک خط برای لاگ / گزارش چندخطی برای `hs2 tune` |
| `AvailableCC` / `AvailableQdisc` | `tune/tune.go:486-513` | بررسی دسترس‌پذیری (با اثر جانبی modprobe) |
| `writeQdiscWithFallback` | `tune/tune.go:530-543` | خواسته → fq_codel → fq |
| `undoLegacyPortRange` | `tune/tune.go:547-561` | برگرداندن `ip_local_port_range` فقط اگر دقیقاً مقدار قدیمی hs2 باشد |
| `readSysctl`/`writeSysctl`/`modprobe` | `tune/tune.go:563-588` | دسترسی مستقیم به `/proc/sys`؛ modprobe از `/sbin/modprobe` یا PATH |

### ۲.۲ فایل وضعیت و `hs2 status` (`cmd/hs2/status.go`)

| جزء | مکان | توضیح |
|---|---|---|
| `statusInterval = 2s` | `status.go:29` | دورهٔ نوشتن |
| `statusRunDir = "/run/hs2"` | `status.go:33` | tmpfs؛ با reboot پاک می‌شود |
| `type liveStatus` | `status.go:37-177` | JSON تخت (نصاب با grep/sed می‌خواند) |
| `statusPath` | `status.go:182-190` | مسیر مطلق پیکربندی، `/`→`-`، فاصله→`_`، + `.status.json` (با `status_path` نصاب در `install.sh:2520-2525` یکسان؛ آزمون `TestStatusPathMatchesInstaller`) |
| `warmPath`/`readWarm`/`warmWriter`/`warmAfter` | `status.go:195-246` | فایل `.warm` برای «شروع گرم» پس از ری‌استارت |
| `startStatusWriter` | `status.go:250-349` | goroutine نویسنده؛ هنگام پایان ctx فایل را حذف می‌کند |
| `cpuMeter` | `status.go:355-404` | CPU خود فرایند از `/proc/self/stat` (درصد یک هسته) |
| `writeStatusFile` | `status.go:408-418` | نوشتن اتمی (tmp + rename) |
| `statusCmd`/`printStatus` | `status.go:422-536` | دستور `hs2 status [-watch]` |
| `patternLine`/`effectiveCeiling`/`ceilingWhy`/`ceilingLine`/`unreported`/`driftLine`/`hwWhy`/`whyLine`/`trafficLine` | `status.go:549-774` | رندر خطوط |
| `role`/`direction`/`carrierName`/`transportLabel`/`endpointLabel` | `status.go:780-829` | برچسب‌ها از پیکربندی |
| `tcpMemWatch` | `status.go:834-855` | پایش `tcp_mem` و تنظیم سیگنال فشار حافظهٔ موتور |
| `cpuLine`/`poolLine`/`sendingLine`/`offloadLine` | `status.go:860-911` | خطوط CPU و مرحلهٔ ارسال dgtun |

### ۲.۳ پایش CPU میزبان (`cmd/hs2/hostcpu.go`)

| جزء | مکان | توضیح |
|---|---|---|
| `type hostMeter` | `hostcpu.go:29-40` | نمونه‌بردار کل سرور |
| ثابت‌های اشباع | `hostcpu.go:58-65` | 90/40/3 برای ورود، 75/20/5 برای خروج |
| `readProcStat` | `hostcpu.go:78-109` | خط تجمعی `cpu` و شمار خطوط `cpuN` |
| `hostShares` | `hostcpu.go:113-124` | busy=(کل−idle−iowait)/کل؛ softirq و steal هم درصدی از کل |
| `readPSI` | `hostcpu.go:128-155` | `some avg10/avg60` از `/proc/pressure/cpu` |
| `readOutDiscards` | `hostcpu.go:159-183` | `Ip: OutDiscards` از `/proc/net/snmp` با نام ستون |
| `sample`/`judge` | `hostcpu.go:187-242` | حالت اشباع با پسماند (hysteresis) |
| `hostCPUText` | `hostcpu.go:246-268` | متن «`92% busy across 2 core(s) (softirq 11%, steal 3%), tasks waited for a core 58% of the last 10 s`» |

### ۲.۴ `hs2 doctor` (`cmd/hs2/doctor.go` و `doctor_cert.go`)

| بررسی (نام در خروجی) | تابع | مکان |
|---|---|---|
| `binary` | `versionLine` | `doctor.go:39` |
| `config` | `checkConfig` (همان `hs2 check`) | `doctor.go:41-53` |
| `running` (+ `refill`) | `checkRunning` | `doctor.go:119-139` |
| `endpoint` | `checkEndpoint` | `doctor.go:151-167` |
| `certificate` | `checkCert` | `doctor.go:183-212` |
| `cert renewal` | `checkCertRenewal` | `doctor_cert.go:240-304` |
| `tun <iface>` | `checkTun` | `doctor.go:218-248` |
| `kernel tuning` | `checkTuning` + `doctorTunePlan` | `doctor.go:274-309,415-432` |
| `link pool ceiling` | `checkLinkPool` | `doctor.go:325-399` |
| `link pool (other server)` / `link count visibility` | `checkManyLinks` | `doctor.go:457-487` |
| `user ports` | `checkPorts` | `ports.go:473+` |
| `kernel TCP memory` | `checkTCPMem` | `doctor.go:521-537` |
| `conntrack table` | `checkConntrack` | `doctor.go:575-592` |
| `server cpu` | `checkCPU` | `doctor.go:599-647` |
| `tunnels together` | `checkTunnelsTogether` | `doctor.go:653-697` |
| `clock` | (اطلاع) | `doctor.go:77-78` |

`doctorReport` (`doctor.go:89-114`): هر خط «`  [tag] name: detail`»؛ برچسب‌ها ` ok `، `warn`، `fail`، `info`؛ کد خروج ۱ فقط وقتی `fails > 0` (`doctor.go:82-84`).

### ۲.۵ قطعات مرتبط در `cmd/hs2/main.go`

| جزء | مکان | توضیح |
|---|---|---|
| `MaxLinks` سه‌حالته + `maxLinksSet` | `main.go:65-71,110-121` | ۰ = خودکار، عدد = ثابت، غایب/null = ۳۲ تاریخی |
| `Tuning *tune.Config` | `main.go:104` | بخش اختیاری |
| `applyTuning` (متغیرهای `HS2_TUNE_*`) | `main.go:258-275` | فقط برای آزمایشگاه |
| `warmLinks` | `main.go:280-293` | خواندن فایل warm و اعمال روی اندازهٔ شروع |
| `setMemoryLimit` | `main.go:309-320` | حد نرم حافظهٔ Go = نصف RAM |
| اعمال tune در `runCmd` | `main.go:351-362` | Plan → Apply یا فقط لاگ؛ cc به سوکت‌های لینک؛ خط سقف |
| `buildTunePlan` / `tuneSkipReason` / `tuneCmd` | `main.go:552-592` | |
| `recommendLinksCmd` | `main.go:604-624` | خروجی عدد خام (نصاب) یا با `--why` |
| `linkEnvelope` | `main.go:644-657` | min پیش‌فرض ۲، per_link پیش‌فرض ۸، max ≥ min |
| `detectHW` (متغیر، برای pin در آزمون) | `main.go:661` | = `tune.Detect` |
| `ceilAuto/ceilFixed/ceilDefault/ceilICMP` | `main.go:664-669` | |
| `icmpMaxLinks = 8` / `isICMPTun` | `main.go:678-683` | |
| `legacyMaxLinks = 32` | `main.go:686` | |
| `linkCeiling` | `main.go:711-724` | |
| `ceilingLogLine` | `main.go:729-753` | خط شروع «`link pool: ceiling N links — …`» |

---

## ۳. جریان داده و کنترل، گام‌به‌گام

### ۳.۱ شروع daemon (`hs2 run -c cfg`)

1. `startPprof()` اگر `HS2_PPROF` آدرس loopback باشد (`main.go:228`، `pprof.go:20-43`).
2. `applyTuning()`: متغیرهای محیطی `HS2_TUNE_NOTSENT`، `HS2_TUNE_SMUX_FRAME`، `HS2_TUNE_SMUX_STREAMBUF`، `HS2_TUNE_SMUX_SESSBUF`، `HS2_TUNE_CC` روی متغیرهای سراسری `tlscarrier`/`engine` نوشته می‌شوند (`main.go:258-275`).
3. `setMemoryLimit()`: اگر `GOMEMLIMIT` تعریف نشده باشد، `debug.SetMemoryLimit(RAM/2)` و لاگ «`memory: Go soft limit %d MB (half of %d MB RAM; set GOMEMLIMIT to override)`» (`main.go:309-320`).
4. خواندن پیکربندی؛ رد کردن `bind_local_ip` نامعتبر (`main.go:332-344`).
5. `plan := buildTunePlan(fc)` ← `tune.Detect()` + `tune.Build(cfg, ram, cpus, tune.AvailableCC, tune.AvailableQdisc)` (`main.go:552-559`). توجه: همین ساختن Plan با `AvailableCC/AvailableQdisc` ممکن است modprobe اجرا کند.
6. اگر `euid==0` و `HS2_NO_TUNE==""` ← `plan.Apply(log.Printf)`؛ وگرنه فقط «`tuning: <summary> (not applied: not root|HS2_NO_TUNE set)`» (`main.go:352-356`).
7. اگر `HS2_TUNE_CC` تعریف نشده و `plan.Congestion` خالی نیست ← `tlscarrier.CongestionControl = plan.Congestion` (`main.go:357-359`). یعنی cc انتخاب‌شده (حتی در mode=off) روی **هر سوکت لینک** با `TCP_CONGESTION` اعمال می‌شود (`tlscarrier/tune_linux.go:47-49`).
8. اگر پول لینک دارد ← چاپ `ceilingLogLine(fc)` (`main.go:360-362`).
9. dgtun/udp/auto بدون MTU ← 1280 (`main.go:367-369`). برای l3mtcp/tls، MTU پیش‌فرض hs0 = **1380** (`main.go:458-461`).
10. انتخاب حامل (`main.go:405-424`). در `runStream`: TUN (برای l3mtcp) **بعد از** اعمال tune باز می‌شود، پس `default_qdisc` روی hs0 اثر دارد (توضیح `tune/tune.go:307-308`). `tun.Open` برای hs0 `txqueuelen 2000` می‌گذارد (`tun/tun_linux.go:125`).
11. لبه: `linkEnvelope` ← `IranConfig{Min,Max,PerLink,…, OnStart: startStatusWriter}`؛ `WarmLinks = warmLinks(...)` (`main.go:471-485`). خروجی: `exitMax` = سقف حل‌شدهٔ همین سرور که به لبه گزارش می‌شود (در حالت direct اعمال نمی‌شود) (`main.go:509-516`). خروجی معکوس: `RevLinks = engine.WarmSize(min,max)` (`main.go:530`).
12. موتور یک‌بار `OnStart(statsFn)` را صدا می‌زند (`engine/stream_iran.go:130-131`، `engine/stream_kharej.go:107-108`، `engine/stream_reverse.go:136-137`، `engine/dgpool.go:2210-2211,2247-2248`) ← `startStatusWriter`.

### ۳.۲ برنامهٔ `Apply` (`tune/tune.go:398-448`)

1. `undoLegacyPortRange`: اگر `ip_local_port_range` (پس از یکسان‌سازی فاصله‌ها) دقیقاً `"10240 65535"` باشد، به `"32768 60999"` برمی‌گرداند و یادداشت می‌گذارد (`:405-407,547-561`).
2. `modprobe tcp_<cc>` و `modprobe sch_<qdisc>` (`:409-414`).
3. برای هر sysctl به ترتیب: اگر `net.core.default_qdisc` بود ← `writeQdiscWithFallback` (خواسته → fq_codel → fq، هرکدام با modprobe)؛ در صورت تغییر، Plan و یادداشت به‌روز می‌شوند (`:420-433`). بقیه با `writeSysctl` (نوشتن مستقیم در `/proc/sys/...`) (`:434-439`).
4. لاگ: یک خط خلاصه، در صورت شکست شمار اعمال‌شده، و هر یادداشت (`:441-447`). هرگز daemon را متوقف نمی‌کند.

### ۳.۳ تیک نویسندهٔ وضعیت (هر ۲ ثانیه، `status.go:279-334`)

1. `tcpMem.check()`: خواندن `/proc/net/sockstat` (`TCP: … mem N`) و `/proc/sys/net/ipv4/tcp_mem`؛ گذار بالا/پایین با لاگ؛ **`engine.SetTCPMemPressure(w.above)`** (`status.go:840-855`).
2. `s := stats()` (عکس فوری `engine.PoolStats`).
3. شمارش لینک و هدف و بازه؛ اگر `CfgMax>0`: `PeerMax`، `EffMax/LimitBy` از `effectiveCeiling`، و `CeilingText = ceilingLine(ls)` (`:283-289`).
4. اگر پورت کاربر دارد: `fillPeerRoutes` و `PortsLines` (`:290-293`).
5. ترافیک: `Users`، `Mbit` (گرد به یک رقم)، `Sat`، `Counted`، `Flowing`، `PeakMbit` (`:294-295`).
6. اگر `s.Max>0` و فاز نه `following` و نه `listening` (یعنی پول لبه): `warm.note(s.Target)`، و جزئیات serving/retiring/held/pressed/cap/reason/next_probe/exit_stats/refill (`:299-308`).
7. اگر datagram: همهٔ شمارنده‌های dgtun (`:309-322`).
8. `cpu.sample()` و `host.sample(cpuPct)`؛ **`udpcarrier.SetHostSaturated(hs.Saturated)`** (`:323-330`).
9. `CertDays = firstCertExpiryDays()` (`cert.go:146-156`)، `Updated = now`، `writeStatusFile` (`:331-333`).
10. پایان ctx ← `os.Remove(path)` (`:341-343`). فایل `.warm` **حذف نمی‌شود** (تا ری‌استارت بعدی از آن استفاده کند).

### ۳.۴ چرخهٔ فایل warm (شروع گرم)

- نوشتن: `warmWriter.note(target)` فقط اگر `target≥1`، و فرایند حداقل `warmAfter` (۱ دقیقه) زنده بوده، و (هدف تغییر کرده یا ≥۱ دقیقه از نوشتن قبلی گذشته) (`status.go:238-246`). دلیل یک‌دقیقه: حلقهٔ crash (`Restart=always`، `RestartSec=3` در `install.sh:840-841`) مقدار بالا را تا ابد تازه نکند (`status.go:232-236`).
- خواندن: `readWarm` اگر سن فایل ≤ `warmMaxAge` (۱۵ دقیقه) و عدد ≥۱ (`status.go:201-220`).
- اعمال: `warmLinks` عدد را در `[min,max]` می‌گیرد و **فقط اگر از `engine.WarmSize(min,max)` بزرگ‌تر باشد** استفاده می‌کند (فقط اندازهٔ شروع را بالا می‌برد) و لاگ «`link pool: coming up at %d links, the size it had before this restart (the autopilot resizes it from there)`» (`main.go:280-293`). `WarmSize` = `warmStartLinks` (۸) محدود به `[min,max]` (`engine/health.go:85`، `engine/linkmanager.go:742-751`).
- reboot: `/run` tmpfs است ← شروع سرد. حذف تونل در نصاب فایل warm را پاک می‌کند (`install.sh:2386-2387,2436`).

### ۳.۵ `hs2 status` (`status.go:422-536`)

فقط فایل را می‌خواند (هیچ ارتباطی با daemon ندارد). ترتیب خطوط:
`hs2 — role · dir · transport[ (stale…)]` ← endpoint ← `links:` ← `ceiling:` ← `ceiling: note —` (drift) ← `why:` ← `refill:` ← `ports:` ← `traffic:` (اگر Users>0 یا Mbit>0 یا سمت خارج) ← `exit stats:` (اگر نه ok) ← بلوک dgtun (`loss/fec/policer/packets/drops/pool/sending/tun/mute/carriers/reorder`، فقط اگر حامل با `dgtun` شروع شود یا Loss/Parity/Policed غیرصفر باشد) ← `cpu:` ← `net:` (OutDiscards) ← `certificate:`.
`--watch`: پاک کردن صفحه با `\033[H\033[2J` و تکرار هر ۲ ثانیه، بی‌پایان (`status.go:436-440`).

### ۳.۶ `hs2 doctor` (`doctor.go:29-85`)

ترتیب دقیق در بخش ۲.۴ آمده. نکات کلیدی جریان:
- اگر پیکربندی JSON نباشد ← همهٔ بررسی‌های زنده با یک `info` رد می‌شوند (`:70-72`).
- `checkCPU` واقعاً **۱ ثانیه می‌خوابد** (`:68`) تا دو نمونه از `/proc/stat` بگیرد.
- هیچ‌چیز تغییر نمی‌کند: `doctorTunePlan` از `AvailableCC/AvailableQdisc` (که modprobe می‌کنند) استفاده **نمی‌کند** و فقط `/proc` را می‌خواند (`:406-432`)؛ تجدید گواهی certbot را اجرا نمی‌کند (`doctor_cert.go:26-28`).

### ۳.۷ تصمیم سقف لینک (کامل)

```
linkCeiling(fc):                                  cmd/hs2/main.go:711-724
  max_links > 0           → max_links ، "fixed"
  dgtun روی icmp          → 8 ، "icmp"            (حتی اگر max_links=0 یا غایب)
  max_links == 0 (صریح)   → tune.RecommendedMaxLinks(ram,cpus) ، "auto"
  غایب / null             → 32 ، "default"
linkEnvelope(fc):                                 cmd/hs2/main.go:644-657
  min = min_links || 2 ؛ per = per_link || 8 ؛ max = max(linkCeiling, min)
```

```
maxLinksRule(ram, cpus):                          tune/tune.go:235-251
  base = 32|48|64 بر اساس profileFor
  cpus < 2                    → base  ("single core")
  byRAM = ram / 48
  byRAM > 300                 → 300   ("cap")
  cpus < 4 && byRAM > 128     → 128   ("few cores")
  byRAM <= base               → base  ("profile")
  وگرنه                        → byRAM ("ram")
```

سقف مؤثر تونل (`effectiveCeiling`، `status.go:584-606`): direct ← سقف سرور ایران به‌تنهایی (خروجی هر لینکی را که ایران بزند می‌پذیرد)؛ reverse ← کمینهٔ دو سرور (لبه هدف را به max خودش و خروجی آن هدف را به max خودش می‌بُرد)؛ نامعلوم ← `0,""`. این تابع **فقط نمایشی** است؛ اجرای واقعی در موتور است.

---

## ۴. جدول ثابت‌ها، آستانه‌ها، اندازهٔ بافرها و زمان‌سنج‌ها

### ۴.۱ پروفایل‌ها و sysctlها (`tune`)

| نام | مقدار | مکان | معنی |
|---|---|---|---|
| آستانهٔ high (۱) | `ram>=4096 && cpus>=4` | `tune/tune.go:161` | |
| آستانهٔ high (۲) | `ram>=4096 \|\| (ram>=2048 && cpus>=4)` | `tune/tune.go:163` | هسته‌ها جعبهٔ مرزی را بالا می‌برند |
| آستانهٔ medium | `ram>=1536` | `tune/tune.go:165` | |
| low | بقیه | `tune/tune.go:167` | |
| low: rmem_max/wmem_max | `8<<20` (8388608) | `tune/tune.go:287` | |
| low: netdev_max_backlog / somaxconn | 2048 / 1024 | `tune/tune.go:287` | |
| medium: rmem/wmem | `16<<20` (16777216) | `tune/tune.go:285` | |
| medium: backlog / somaxconn | 8192 / 4096 | `tune/tune.go:285` | |
| high: rmem/wmem | `32<<20` (33554432) | `tune/tune.go:283` | |
| high: backlog / somaxconn | 16384 / 8192 | `tune/tune.go:283` | |
| `net.ipv4.tcp_congestion_control` | `bbr` (یا جایگزین) | `tune/tune.go:299,306,338-340` | زنجیره: خواسته → bbr → cubic |
| `net.core.default_qdisc` | `fq_codel` (یا جایگزین) | `tune/tune.go:300,309,341-343` | زنجیره: خواسته → fq_codel → fq |
| `net.core.rmem_max` | rmemMax پروفایل | `tune/tune.go:344` | |
| `net.core.wmem_max` | wmemMax پروفایل | `tune/tune.go:345` | |
| `net.ipv4.tcp_rmem` | `"4096 131072 <rmemMax>"` | `tune/tune.go:346` | |
| `net.ipv4.tcp_wmem` | `"4096 65536 <wmemMax>"` | `tune/tune.go:347` | |
| `net.core.netdev_max_backlog` | backlog پروفایل | `tune/tune.go:348` | |
| `net.core.somaxconn` | somaxconn پروفایل | `tune/tune.go:349` | |
| `net.ipv4.tcp_max_syn_backlog` | = somaxconn | `tune/tune.go:350` | |
| `net.ipv4.tcp_notsent_lowat` | `131072` (۱۲۸ KiB) | `tune/tune.go:352` | برای برنامه‌های غیرتونل؛ سوکت‌های لینک مقدار خودشان (۳۲ KiB) را دارند |
| `net.ipv4.tcp_slow_start_after_idle` | `0` | `tune/tune.go:353` | |
| `net.ipv4.tcp_mtu_probing` | `1` | `tune/tune.go:354` | |
| `net.ipv4.tcp_fin_timeout` | `20` | `tune/tune.go:355` | |
| `net.ipv4.tcp_tw_reuse` | `1` | `tune/tune.go:356-362` | پیش‌فرض هسته (۲) فقط روی loopback؛ forwarder‌های dgtun و خروجی با پنل غیر 127.x |
| `net.ipv4.conf.all.rp_filter` / `default.rp_filter` | `2` (loose) | `tune/tune.go:367-370` | سرور چند-IP |
| شمار sysctlها در حالت auto | **۱۶** (با cc و qdisc) | محاسبه از `Build`؛ اجرا شد | `profile high … bbr + fq_codel, 16 sysctls` |
| `legacyPortRange` | `"10240 65535"` | `tune/tune.go:548` | مقداری که نسخهٔ قدیمی hs2 می‌گذاشت |
| `defaultPortRange` | `"32768 60999"` | `tune/tune.go:549` | پیش‌فرض لینوکس |
| qdiscهای «همیشه موجود» | `fq, fq_codel, pfifo_fast, sfq` | `tune/tune.go:505-508`، `doctor.go:425-428` | |

### ۴.۲ سقف لینک

| نام | مقدار | مکان | معنی |
|---|---|---|---|
| `maxLinksLow/Medium/High` | 32 / 48 / 64 | `tune/tune.go:199-201` | کف (هرگز کمتر از مقدار قبلی) |
| `LinkWorstCaseMiB` | 12 | `tune/tune.go:206` | 8 MiB بافر نشست smux × ۱٫۵ گردکردن توان-دو |
| `LinkRAMPerLinkMB` | `4*12 = 48` | `tune/tune.go:208` | تا بدترین حالت ≤ ۲۵٪ RAM |
| `MaxLinksCap` | 300 | `tune/tune.go:210` | |
| `maxLinksFewCores` | 128 | `tune/tune.go:212` | ۲ تا ۳ هسته |
| `legacyMaxLinks` | 32 | `cmd/hs2/main.go:686` | `max_links` غایب |
| `icmpMaxLinks` | 8 | `cmd/hs2/main.go:678` | dgtun روی icmp |
| min پیش‌فرض / per_link پیش‌فرض | 2 / 8 | `cmd/hs2/main.go:646-655` | |
| `maxWireLinks` | 65535 | `cmd/hs2/check.go:452` | u16 روی سیم |
| `maxSaneLinks` | 1024 | `cmd/hs2/check.go:453` | بالاتر = هشدار |
| `minLinksHigh` | 64 | `cmd/hs2/check.go:477` | min_links بالاتر = هشدار |
| `manyLinksAt` | 64 | `cmd/hs2/doctor.go:452` | بالاتر = یادداشت دیدپذیری |
| `engine.SmuxSessionBuffer` | `8<<20` | `engine/mtcp_link.go:264` | پایهٔ `LinkWorstCaseMiB` |
| `engine.SmuxStreamBuffer` | `2<<20` | `engine/mtcp_link.go:262` | |
| `engine.SmuxFrameSize` | `16<<10` | `engine/mtcp_link.go:258` | |
| `warmStartLinks` | 8 | `engine/health.go:85` | اندازهٔ شروع بدون تاریخچه |

**جدول نمونهٔ سقف خودکار (اجرا شده با کد واقعی):**

| RAM (MB) | هسته | پروفایل | سقف | دلیل (`MaxLinksReason`) |
|---|---|---|---|---|
| 512 | 1 | low | 32 | single core |
| 1024 | 1 | low | 32 | single core |
| 1024 | 2 | low | 32 | profile |
| 1536 | 2 | medium | 48 | profile |
| 1950 | 1 | medium | 48 | single core |
| 2048 | 2 | medium | 48 | profile |
| 2048 | 4 | high | 64 | profile |
| 3072 | 2 | medium | 64 | ram |
| 3800 | 2 | medium | 79 | ram |
| 3800 | 4 | high | 79 | ram |
| 4096 | 1 | high | 64 | single core |
| 4096 | 2 | high | 85 | ram |
| 6144 | 2 | high | 128 | ram |
| 8192 | 2 | high | 128 | few cores |
| 8192 | 4 | high | 170 | ram |
| 12288 | 4 | high | 256 | ram |
| 14400 | 4 | high | 300 | ram (نقطهٔ رسیدن به ۳۰۰) |
| 16384 | 4 | high | 300 | cap |
| 17408 | 20 | high | 300 | cap (سرور ایران تولید) |
| 22528 | 12 | high | 300 | cap (سرور خارج تولید) |

### ۴.۳ وضعیت زنده، CPU و حافظه

| نام | مقدار | مکان | معنی |
|---|---|---|---|
| `statusInterval` | 2s | `status.go:29` | دورهٔ نوشتن فایل و نمونه‌برداری CPU/حافظه |
| `statusRunDir` | `/run/hs2` | `status.go:33` | |
| حد «کهنه» در `hs2 status` و doctor | `age > 6` ثانیه | `status.go:456`، `doctor.go:131,343,464,630,662` | |
| حد «تازه» در `hs2 ports` | `≤ 7` ثانیه | `ports.go:256` | |
| حد «تازه» در نصاب | `≤ 7` ثانیه | `install.sh:2527-2533` | |
| `warmMaxAge` | 15min | `status.go:201` | |
| `warmAfter` | 1min | `status.go:236` | |
| بازنویسی warm بدون تغییر | هر ≥1min | `status.go:239` | سن فایل نشان‌دهندهٔ بالا بودن تونل |
| `clkTck` | 100 | `status.go:365` | USER_HZ |
| cpuMeter: داغ | `pct >= 90*cores` برای ۳ نمونه | `status.go:391-395` | لاگ گلوگاه CPU |
| cpuMeter: سرد | `pct < 70*cores` | `status.go:396-401` | |
| `hostSatBusy` | 90.0 | `hostcpu.go:59` | ٪ همهٔ هسته‌ها |
| `hostSatPSI` | 40.0 | `hostcpu.go:60` | PSI some avg10 (در doctor: avg60) |
| `hostSatRuns` | 3 | `hostcpu.go:61` | ≈ ۶ ثانیه |
| `hostClearBusy` | 75.0 | `hostcpu.go:62` | |
| `hostClearPSI` | 20.0 | `hostcpu.go:63` | |
| `hostClearRuns` | 5 | `hostcpu.go:64` | ≈ ۱۰ ثانیه |
| tcpMem: ورود به فشار | `mem >= tcp_mem[1]` | `status.go:847` | |
| tcpMem: خروج | `mem < tcp_mem[1]*9/10` | `status.go:850` | پسماند ۱۰٪ |
| حد نرم حافظهٔ Go | `RAM/2` | `main.go:317` | مگر `GOMEMLIMIT` |
| `pacerSatCredit` | 50ms | `udpcarrier/pacer.go:120` | مصرف‌کنندهٔ سیگنال اشباع |

### ۴.۴ doctor

| نام | مقدار | مکان | معنی |
|---|---|---|---|
| مهلت اتصال TCP به نقطهٔ پایانی | 8s | `doctor.go:160` | شامل DNS؛ دو بار از دست رفتن SYN را تحمل می‌کند |
| گواهی: هشدار | `days <= 7` | `doctor.go:206`، `status.go:539` | |
| گواهی: شکست | منقضی | `doctor.go:204-205` | |
| تجدید certbot: «عقب افتاده» | `daysLeft < threshold-2` | `doctor_cert.go:274` | تایمر دو بار در روز |
| آستانهٔ تجدید | `renew_before_expiry` یا ⅓ عمر (اگر عمر <10 روز: ½)؛ پیش‌فرض 30 | `doctor_cert.go:67-80` | |
| گواهی خارج از certbot | هشدار اگر `< 30` روز | `doctor_cert.go:248` | |
| پورت HTTP-01 | 80 مگر `http01_port` | `doctor_cert.go:110,136-139` | |
| conntrack | هشدار ≥ 70٪ | `doctor.go:587` | |
| kernel TCP memory | ≥ hard: warn، ≥ pressure: warn | `doctor.go:530-533` | |
| server cpu: warn | `busy>=90 \|\| psi60>=40` | `doctor.go:640` | |
| server cpu: info | `busy>=75 \|\| psi60>=20` | `doctor.go:642` | |
| tunnels together: warn | بدترین حالت > ۴۰٪ RAM | `doctor.go:691` | پیشنهاد کاهش به حدود `100/n`٪ |
| نمایش ناهمخوانی tuning | حداکثر ۳ مورد + «(+N more)» | `doctor.go:298-305` | |
| watchCerts | هر 1min؛ هشدار ≤7 روز | `cert.go:118-142` | |

### ۴.۵ سوکت لینک (اعمال‌شده جدا از sysctl، مرتبط مستقیم با tune)

| نام | مقدار | مکان | معنی |
|---|---|---|---|
| `NotSentLowat` | `32<<10` | `tlscarrier/tune_linux.go:19` | آزمایشگاه: ۱۶–۳۲ KiB بهترین؛ ≥۶۴ KiB تأخیر افزود؛ خاموش ۳ تا ۷ برابر بدتر |
| `UserTimeoutMs` | 20000 | `tlscarrier/tune_linux.go:22` | شکست لینک سیاه‌چاله‌ای |
| `CongestionControl` | `"bbr"` (با Plan بازنویسی می‌شود) | `tlscarrier/tune_linux.go:29`، `main.go:357-359` | |
| `TCP_NODELAY` | روشن | `tlscarrier/tune_linux.go:37` | |
| smux keepalive | تصادفی 4–8s، timeout 24s | `engine/mtcp_link.go:278-279` | |
| MPTCP روی شنونده | خاموش | `engine/listen.go:41` | چون MPTCP پذیرفته‌شده `tcp_notsent_lowat` را نادیده می‌گیرد (`CHANGELOG.md:827-831`) |
| hs0: MTU / txqueuelen | 1380 / 2000 | `main.go:458-461`، `tun/tun_linux.go:125` | |
| `l3MaxSojourn` (کانال جانبی) | 60ms | `engine/l3_link.go:62` | |

---

## ۵. حلقه‌های کنترلی

| حلقه | ورودی | شرط | خروجی | دوره |
|---|---|---|---|---|
| نویسندهٔ وضعیت | `statsFn()`، `/proc` | — | فایل JSON | 2s (`status.go:336`) |
| tcpMemWatch (پسماند) | `/proc/net/sockstat`، `tcp_mem` | بالا: `mem≥press`؛ پایین: `mem<0.9·press` | لاگ یک‌باره + **`engine.SetTCPMemPressure`** | هر تیک وضعیت |
| hostMeter (پسماند) | `/proc/stat`، `/proc/pressure/cpu`، `/proc/net/snmp` | داغ: busy≥90 یا psi10≥40، ۳ بار؛ سرد: busy<75 و psi10<20، ۵ بار؛ میان دو حد: هر دو شمارنده صفر | لاگ + **`udpcarrier.SetHostSaturated`** + فیلد وضعیت | هر تیک |
| cpuMeter | `/proc/self/stat` | ≥90٪·cores سه بار (بدون نمونهٔ <70٪ میانشان) | لاگ یک‌باره ورود/خروج | هر تیک |
| warmWriter | `s.Target` | بعد از ۱ دقیقه عمر؛ تغییر یا ≥۱ دقیقه | فایل `.warm` | هر تیک |
| watchCerts | فایل گواهی | تغییر روی دیسک / ≤۷ روز | بارگذاری دوباره / لاگ | 1min (`cert.go:119`) |
| `hs2 status --watch` | فایل وضعیت | — | چاپ | 2s |
| Apply tune | Plan | — | sysctlها | **فقط یک بار در شروع**؛ هیچ اعمال دوره‌ای وجود ندارد |

نکته: نمونهٔ اول hostMeter و cpuMeter هیچ بازه‌ای ندارد (۰ برمی‌گرداند)؛ نمونه‌ای که «چیزی اندازه نگرفت» (نه PSI و نه بازهٔ معتبر) وضعیت اشباع را تغییر نمی‌دهد (`hostcpu.go:219-222`). کاهش شمارندهٔ OutDiscards (برگشت) پایهٔ تازه حساب می‌شود (`hostcpu.go:200-209`).

---

## ۶. حالت‌ها و گذارها، خطاها و بازیابی

### ۶.۱ حالت‌های سقف (`ceil*`)
- `auto` ← از سخت‌افزار در هر شروع؛ هرگز drift ندارد (`status.go:693`).
- `fixed` ← عدد اپراتور؛ drift اگر ≠ پیشنهاد سخت‌افزار.
- `default` ← غایب (۳۲)؛ drift با پیشنهاد «max_links را 0 کنید».
- `icmp` ← ۸؛ drift ندارد؛ `fixed` بالای ۸ روی icmp هشدار می‌گیرد.
- اگر `min_links > سقف` ← سقف تا min بالا می‌رود و همه‌جا «raised to min_links» گفته می‌شود؛ drift روی **سقف خام** (`CeilRaw`) قضاوت می‌شود (`status.go:689-692`).
- خارجِ direct: سقف خودش اعمال نمی‌شود ← هیچ drift یا هشدار سقفی (`status.go:696-698`، `doctor.go:351-354`).

### ۶.۲ حالت‌های `checkLinkPool` در doctor (`doctor.go:325-399`)
- پول ندارد ← info.
- خارج direct ← info «Iran server's ceiling applies».
- icmp خودکار ← ok؛ icmp ثابت >۸ ← warn؛ icmp ثابت ≤۸ ← ok.
- auto: اگر daemon در حال اجرا (`CfgMax` از فایل تازه) و «شروع اکنون» کمتر بدهد ← **warn** «restart … lower ceiling»؛ بیشتر بدهد ← info؛ برابر ← ok.
- ثابت/پیش‌فرض: برابر پیشنهاد ← ok؛ بیشتر ← warn؛ کمتر ← info.

### ۶.۳ خطا و بازیابی در tune
- cc موجود نیست ← bbr ← cubic ← «leaving the kernel default» و هیچ sysctl cc نوشته نمی‌شود (`tune/tune.go:376-393,338`).
- qdisc: `AvailableQdisc` تقریباً همیشه true است؛ جایگزینی واقعی در زمان نوشتن (`writeQdiscWithFallback`).
- sysctl رد شد (کانتینر، کرنل قدیمی) ← یادداشت، ادامه.
- ریشه نیست یا `HS2_NO_TUNE` ← فقط لاگ Plan.
- `mode=off` ← هیچ sysctl؛ cc همچنان برای سوکت‌های تونل انتخاب می‌شود.

### ۶.۴ نویسندهٔ وضعیت
- اگر `MkdirAll(/run/hs2)` شکست بخورد ← **کل goroutine شروع نمی‌شود** (`status.go:252-254`) — نه فایل، نه سیگنال‌های فشار حافظه/اشباع (به بخش ۱۳ نگاه کنید).
- خطای نوشتن فایل نادیده گرفته می‌شود (`status.go:409-417`).
- پایان ctx ← حذف فایل وضعیت.

---

## ۷. قالب فایل وضعیت (پروتکل داخلی بین daemon، CLI و نصاب)

فایل JSON تخت؛ فیلدها **فقط اضافه می‌شوند** تا خوانندهٔ قدیمی کار کند (`engine/linkmanager.go:2214-2215`). کلیدها (`status.go:37-177`):

- **پایه:** `role` («Iran side»/«Kharej side»)، `dir`، `carrier`، `transport`، `encap` (فقط dgtun)، `endpoint`، `links`، `target`، `min`، `max`، `users`، `mbit`، `counted`، `phase` (steady/scaling/probing/holding/shrinking؛ خروجی: following/listening)، `sat`، `cert_days` (−1 = ندارد)، `pid`، `updated` (ثانیهٔ یونیکس).
- **پول لبه:** `serving` (اشاره‌گر؛ حضورش یعنی فایل لبه)، `retiring`، `held_by`، `held_active`، `flowing`، `pressed`، `cap_mbit`، `peak_mbit`، `reason`، `refill`، `next_probe_s`، `exit_stats`.
- **سقف:** `cfg_max`، `ceiling_raw`، `ceiling_mode`، `profile`، `rec_max`، `ram_mb`، `peer_max`، `eff_max`، `limit_by` (iran/kharej/both)، `ceiling_text`.
- **مسیریابی پورت:** `peer_routes` (""/known/older/filtered)، `peer_tags`، `peer_ports`، `peer_default`، `peer_udp`، `peer_cut`، `ports_lines`.
- **dgtun:** `loss_pct`، `max_loss_pct`، `parity_pct`، `fec_at_ceiling`، `fec_recovered`، `fec_lost`، `send_refused`، `rx_dropped`، `tun_drops`، `tun_read`، `sent_pkts`، `recv_pkts`، `tun_written`، `drop_no_carrier`، `drop_queue_full`، `drop_aged`، `mute_closed`، `tun_offload`، `tun_reads`، `tun_segs`، `tun_writes`، `tun_pkts`، `tun_mode`، `tun_bad`، `tun_refused`، `carriers`، `reorder_*`، `policed`، `police_confirmed`، `police_cap_mbit`، `share_mbit`، `busy_queue_ms`، `send_mbit`، `send_held_pct`، `fq_wait_ms`، `write_us`، `per_write`.
- **CPU/میزبان:** `cpu_pct` (٪ یک هسته)، `cpu_cores` (هسته‌های قابل استفادهٔ فرایند)، `host_cpu_pct`، `host_softirq_pct`، `host_steal_pct`، `host_cores`، `psi_cpu10`، `psi_cpu60`، `host_saturated`، `host_out_discards`.

فیلدهایی که نصاب می‌خواند (شمارش از `install.sh`): از جمله `updated`، `links`، `target`، `serving`، `retiring`، `phase`، `users`، `flowing`، `mbit`، `peak_mbit`، `sat`، `pressed`، `cap_mbit`، `reason`، `refill`، `exit_stats`، `counted`، `held_by`، `held_active`، `ceiling_text`، `cpu_pct`، `cpu_cores`، `host_*`، `psi_cpu10` و شمارنده‌های dgtun. یعنی **تغییر نام هر کلید، منوی نصاب را می‌شکند**.

فایل `.warm`: یک عدد صحیح و `\n`، نوشتن اتمی (`status.go:242-245`).

قالب خروجی doctor: «`  [ ok ] name: detail`» / `[warn]` / `[fail]` / `[info]` و خط جمع‌بندی (`doctor.go:94-114`):
- «`N problem(s), M warning(s) — the tunnel is likely NOT healthy.`»
- «`0 problems, M warning(s) — worth a look, not necessarily broken.`»
- «`all checks passed.`»

---

## ۸. متن دقیق لاگ‌های مهم و معنی‌شان

### ۸.۱ tune و شروع
| لاگ | مکان | معنی |
|---|---|---|
| `tuning: profile high (RAM 17.0 GB, 20 cpu) — bbr + fq_codel, 16 sysctls` | `tune/tune.go:441,456-457` | خلاصهٔ Plan اعمال‌شده |
| `tuning: off (RAM …, N cpu) — X qdisc, Y congestion on tunnel sockets only, no system sysctls` | `tune/tune.go:452-454` | mode=off |
| `tuning: %d/%d sysctls applied (%d skipped — see notes)` | `tune/tune.go:443` | برخی رد شدند |
| `tuning: note — …` | `tune/tune.go:446` | هر یادداشت |
| یادداشت‌ها: `congestion control "x" not available — using "y"`، `… not available and no fallback found — leaving the kernel default`، `qdisc "x" unavailable on this kernel — using "y"`، `could not set net.core.default_qdisc (leaving the kernel default)`، `could not set <key> (kernel rejected it or no permission)`، `restored net.ipv4.ip_local_port_range to the kernel default 32768 60999 (an earlier hs2 build had widened it)`، `mode=off: system sysctls left untouched (…)` | `tune/tune.go:313,387,391,406,423,430,438` | |
| `tuning: <summary> (not applied: not root\|HS2_NO_TUNE set)` | `main.go:355`، `561-566` | |
| `tuning: HS2_TUNE_X=%d` / `tuning: HS2_TUNE_CC=%q` | `main.go:263,273` | جایگزینی آزمایشگاهی |
| `memory: Go soft limit %d MB (half of %d MB RAM; set GOMEMLIMIT to override)` | `main.go:319` | |
| `link pool: ceiling N links — auto from this server's hardware (…); re-derived at every start` | `main.go:737,748` | |
| `link pool: ceiling N links — fixed by max_links in the config (auto would give M here: …)` (+ هشدار icmp) | `main.go:739-742` | |
| `link pool: ceiling 8 links — tun over icmp: every carrier is one echo id …` | `main.go:744` | |
| `link pool: ceiling 32 links — the default (max_links is not set in the config; 0 = auto would give M here: …)` | `main.go:746` | |
| `… (raised from X to min_links)` | `main.go:749-751` | |
| `link pool: coming up at %d links, the size it had before this restart (the autopilot resizes it from there)` | `main.go:291` | شروع گرم |

### ۸.۲ نویسندهٔ وضعیت
| لاگ | مکان | معنی |
|---|---|---|
| `cpu: hs2 is using %.0f%% of its %d core(s) — the CPU is the bottleneck now, not the path (a bigger VPS, or fewer/other carriers, would carry more)` | `status.go:394` | خود hs2 گلوگاه است |
| `cpu: back to %.0f%% of %d core(s) — no longer the bottleneck` | `status.go:400` | |
| `cpu: the server is saturated — <hostCPUText>; hs2 itself uses %.0f%% of one core. Every carrier now sends late however good the path is: other programs on this server (another tunnel?) or a bigger VPS are the fix, not the tunnel's settings` | `hostcpu.go:230` | کل سرور اشباع |
| `cpu: the server has room again — …` | `hostcpu.go:236` | |
| `kernel TCP memory: %d MB in TCP buffers, above the kernel's pressure mark (%d MB of %d MB, tcp_mem) — every TCP socket's buffers are being squeezed; look for stalled readers (hs2 logs "whose app had taken nothing")` | `status.go:849` | فعال شدن حالت فشار در موتور |
| `kernel TCP memory: back below the pressure mark (%d MB of %d MB)` | `status.go:852` | |
| `cert: %s expires in %d day(s) — renew it now; 'hs2 doctor' (cert renewal) shows whether and how it renews` | `cert.go:137` | |
| `l3: dropped %d packets in 30s on the tun side channel (queue limit or no link) — …` | `engine/l3_link.go:389` | تنها دیدپذیری حذف‌های hs0 در l3mtcp (فقط لاگ) |

### ۸.۳ خروجی‌های مهم `hs2 status`
- `no live status yet (is the tunnel running? — the status file appears a few seconds after start)` (`status.go:446`)
- `  (stale: last updated %ds ago — tunnel may be down)` (`status.go:457`)
- `links:` مثال: `7 up = 5 serving + 2 retiring (shrinking, range 2–32)`، `40 up / target 48, capped at 40 by the Kharej server (steady, range 2–64)` (`status.go:549-571`)
- `ceiling:` مثال‌ها در `status.go:649-671`؛ `ceiling: note —` از `driftLine`.
- `traffic:` مثال: `251 connections (18 active) · 6.1 Mbit/s · 1 link at its limit (~2.4 Mbit/s each)` (`status.go:750-774`).
- `cpu:` مثال: `hs2 140% of one core (2 core(s)) · server 97% busy across 2 core(s) (softirq 12%), tasks waited for a core 81% of the last 10 s — SATURATED: carriers send late however good the path is (see the log)` (`status.go:860-871`).

---

## ۹. گزینه‌های پیکربندی و متغیرهای محیطی

### ۹.۱ پیکربندی
| کلید | مقدار | اثر | مکان |
|---|---|---|---|
| `tuning.mode` | `auto` (پیش‌فرض) / `manual` / `off` | | `tune/tune.go:42`؛ اعتبارسنجی `check.go:352-356` |
| `tuning.congestion` | پیش‌فرض `bbr` | بدون اعتبارسنجی نام؛ با جایگزینی | `tune/tune.go:43` |
| `tuning.qdisc` | پیش‌فرض `fq_codel` | | `tune/tune.go:44` |
| `tuning.rmem_max` / `wmem_max` / `netdev_backlog` / `somaxconn` | بایت/شمار؛ ۰ = پروفایل | فقط در manual؛ در غیر آن هشدار `check` | `tune/tune.go:48-51`، `check.go:357-365` |
| `max_links` | عدد / ۰ / غایب | ثابت / خودکار / ۳۲ | `main.go:65-71` |
| `min_links` | پیش‌فرض ۲ | بالاتر از ۶۴ هشدار | `main.go:646-648`، `check.go:470-471` |
| `per_link` | پیش‌فرض ۸ | | `main.go:653-655` |
| همهٔ این کلیدها از `hs2 config set` قابل تغییرند؛ `max_links auto` = ۰ | | | `config.go:39-62`، آزمون `TestConfigSetMaxLinksAuto` |

### ۹.۲ متغیرهای محیطی
| متغیر | اثر | مکان |
|---|---|---|
| `HS2_NO_TUNE` (غیرخالی) | اعمال sysctlها رد می‌شود؛ cc هنوز روی سوکت‌ها | `main.go:352` |
| `HS2_TUNE_NOTSENT` | `tlscarrier.NotSentLowat` | `main.go:267` |
| `HS2_TUNE_SMUX_FRAME` / `_STREAMBUF` / `_SESSBUF` | بافرهای smux | `main.go:268-270` |
| `HS2_TUNE_CC` (حتی خالی) | cc سوکت لینک؛ Plan آن را بازنویسی نمی‌کند | `main.go:271-274,357` |
| `GOMEMLIMIT` | جایگزین حد نرم RAM/2 | `main.go:310` |
| `HS2_PPROF` | سرور pprof فقط روی loopback | `pprof.go:20-43` |
| `HS2_TUN_OFFLOAD=0` | (dgtun) خاموش کردن offload TUN | `main.go:840` |

### ۹.۳ دستورها
- `hs2 tune [-c cfg] [--apply]` (`main.go:572-592`): بدون `-c` = Plan خودکار. `--apply` ریشه می‌خواهد.
- `hs2 recommend-links [--why] [-c cfg]` (`main.go:604-624`): نصاب عدد خام را می‌گیرد (`install.sh:1761-1765`) و فقط وقتی دستور موفق باشد (نسخهٔ قدیمی «unknown command» را نشان ندهد).
- `hs2 status -c cfg [--watch]`، `hs2 doctor -c cfg`.
- منوی نصاب: Tuning (`install.sh:3179-3222`)، presetهای دستی (`install.sh:3549-3581`)، Link pool (`install.sh:3242+`)، خط سقف زنده از `ceiling_text` (`install.sh:3227-3236`)، Diagnose (`install.sh:3612`).

---

## ۱۰. آزمون‌ها: چه چیزی تضمین می‌شود

### `tune/tune_test.go`
- `TestProfilesScaleWithHardware`: 512/1→low/8MiB، 2048/2→medium/16MiB، 8192/8→high/32MiB.
- `TestRecommendedMaxLinks`: جدول ۱۴ نقطه‌ای (شامل سرورهای تولید ۱۷۴۰۸/۲۰ و ۲۲۵۲۸/۱۲ → ۳۰۰) + جاروب RAM ۱۲۸..۶۵۵۳۶ (گام ۱۳۷) × هسته ۱..۳۲ که تضمین می‌کند: هرگز کمتر از مقدار قدیمی پروفایل، هرگز بیش از ۳۰۰، تک‌هسته ≤۶۴، زیر ۴ هسته ≤۱۲۸، و هر جا از کف بالاتر رفته `m*12 ≤ ram/4 + 12`.
- `TestMaxLinksReason`: متن دقیق دلیل در پنج حالت.
- `TestDefaultsAreBBRFqCodel`، `TestFallbackWhenUnavailable`، `TestManualOverrides`، `TestOffModeSetsNoSysctls`.
- `TestNeverTouchesLocalPortRange`: Plan هرگز `ip_local_port_range` ندارد.
- `TestUndoLegacyPortRange`: فقط `10240\t65535` برگردانده می‌شود.
- `TestBuildSetsTimeWaitReuse`.
- `TestCgroupMemLimit`: v2 (کمینه در مسیر، `max` = بی‌حد)، v1 (hierarchical)، فایل ناخوانا.

### `cmd/hs2/linkpool_test.go`
- `TestLinkCeilingAutoFollowsHardware`، `TestLinkCeilingFixedIsNeverMoved`، `TestLinkCeilingAbsentKeepsHistorical32` (غایب و null → ۳۲)، `TestLinkEnvelopeMinAboveAuto`، `TestCeilingLogLine`.
- `TestEffectiveCeiling` (۱۱ حالت direct/reverse)، `TestCeilingLineBothServers`، `TestCeilingLiftedByMinLinks`، `TestDriftLine`.
- `TestDoctorLinkPool` (auto در حال اجرا با RAM کمتر → warn؛ بیشتر → info؛ ثابت بالا/پایین؛ خارج direct؛ نقل سقف مؤثر؛ tls).
- `TestStatusFileCarriesCeiling` (فیلدهای سقف و `ceiling_text` و فایل warm؛ حامل بدون پول هیچ فیلد سقفی نمی‌نویسد).
- `TestConfigSetMaxLinksAuto`، `TestPatternLineCappedByKharej`، `TestWarmFile` (یک دقیقهٔ اول نمی‌نویسد؛ فقط افزایش اندازهٔ شروع؛ کهنه/خراب → ۰).
- `TestDoctorManyLinks`، `TestVisibilityNotePerCarrier`، `TestCheckWarnsHighMinLinks`.
- `TestDoctorTCPMem`، `TestDoctorConntrackAndTCPMemWatch` (پسماند ۹۰٪ و **هم‌گامی با `engine.TCPMemPressure()`**).
- `TestTunnelsTogetherCountsEffectiveCeilings` (هاب خارج ۴GB با دو تونل direct ۳۰۰تایی → ۶۰۰ و warn).
- `TestStatusNamesTheCoresTheCeilingUses` (هسته‌های cgroup، نه میزبان).
- `TestDgtunAutoCeilingIsTheStreamPools`، `TestICMPTunCeiling`، `TestDoctorICMPCeiling`.

### سایر
- `status_test.go`: `TestStatusPathMatchesInstaller`، `TestLiveStatusRoundTrip`، `TestLiveStatusShrinkingPool` (خروجی کلیدهای لبه فقط در لبه).
- `doctor_test.go`: `TestNormalizeWS`، `TestAddrsContainCIDR`، `TestDoctorReportTally`، `TestCheckCert` (دروازه‌گذاری سمت شنونده/حامل)، `TestCheckEndpointGating`.
- `hostcpu_test.go`: `TestHostProcParsers`، `TestHostMeterSaturation` (۳ نمونه برای ورود، ۵ برای خروج، PSI به‌تنهایی کافی، نمونهٔ اندازه‌نگرفته وضعیت را عوض نمی‌کند)، `TestHostMeterDiscards`، `TestDoctorCPU`، `TestStatusSendStageAndHostLines`.
- `cpu_test.go`: `TestCPUMeter`.
- `kharej_stats_test.go`: `TestKharejStatusCarriesItsOwnCounts`، `TestRefillNoteInStatusAndDoctor`.
- `doctor_cert_test.go`: `TestCertbotLineage`، `TestReadRenewalConf`، `TestCheckCertRenewal`، `TestCheckCertReturnsLeafForRenewal`، `TestListeningOnPort`، `TestCheckCertRenewalHooksAndLifetime`، `TestRenewThresholdDays`، `TestCronRunsCertbot`.
- `ports_test.go`: `TestStatusShowsPorts`، `TestDoctorPorts` و دیگران.

---

## ۱۱. «از قبل وجود دارد» (برای جلوگیری از دوباره‌کاری)

1. تنظیم خودکار sysctl بر اساس RAM/هسته با سه پروفایل، در **هر شروع**، با حالت‌های auto/manual/off، و جایگزینی cc (bbr→cubic) و qdisc (→fq_codel→fq).
2. تشخیص محدودیت **cgroup** (v1 و v2) برای RAM؛ هسته از GOMAXPROCS (آگاه از cgroup).
3. BBR + `TCP_NOTSENT_LOWAT=32KiB` + `TCP_USER_TIMEOUT=20s` + `TCP_NODELAY` روی **هر سوکت لینک**، مستقل از sysctl سیستم؛ MPTCP روی شنونده خاموش.
4. `tcp_notsent_lowat=128KiB` سیستمی، `tcp_slow_start_after_idle=0`، `tcp_mtu_probing=1`، `tcp_fin_timeout=20`، `tcp_tw_reuse=1`، `rp_filter=2`.
5. **عمداً دست‌نزدن** به `ip_local_port_range` + برگرداندن مقدار قدیمی خود hs2.
6. سقف خودکار لینک (۴۸MB به ازای هر لینک، ≤۳۰۰، ≤۱۲۸ زیر ۴ هسته، تک‌هسته = پروفایل، کف ۳۲/۴۸/۶۴)؛ سقف ۸ برای icmp؛ ۳۲ برای پیکربندی قدیمی.
7. نمایش سقف **مؤثر** روی هر دو سرور (direct/reverse) با تبادل سقف طرف مقابل (فقط نمایشی)، drift، و «capped at N by the Kharej server».
8. حد نرم حافظهٔ Go = نصف RAM.
9. فایل وضعیت زنده هر ۲ ثانیه، اتمی، با مسیر قطعی هم‌خوان با نصاب.
10. شروع گرم پس از ری‌استارت (۱۵ دقیقه، فقط افزایش، محافظ حلقهٔ crash).
11. پایش CPU خود فرایند و **کل سرور** (busy، softirq، steal، PSI، OutDiscards) با پسماند، و تغذیهٔ اشباع به pacer datagram.
12. پایش **فشار حافظهٔ TCP هسته** و تغذیه به موتور (نگهبان خوانندهٔ گیرکرده و توقف قضاوت سلامت لینک).
13. doctor با ۱۵ بررسی فقط‌خواندنی: پیکربندی، اجرا، دسترسی TCP به نقطهٔ پایانی (۸ ثانیه)، گواهی و **سلامت تجدید certbot** (standalone/پورت ۸۰/pre-hook/تایمر/cron/DNS-01 دستی)، TUN، ناهمخوانی tuning با `/proc/sys`، سقف لینک، دیدپذیری لینک‌های زیاد، جدول پورت‌ها، tcp_mem، conntrack، CPU سرور، جمع تونل‌ها، ساعت.
14. `hs2 tune` و `hs2 recommend-links --why` و منوهای نصاب (Tuning، presets دستی، Link pool، Diagnose).
15. سرور pprof اختیاری فقط روی loopback.

---

## ۱۲. ایده‌های امتحان‌شده و ردشده (طبق کد/مستندات)

| ایده | سرنوشت | دلیل | منبع |
|---|---|---|---|
| گشاد کردن `ip_local_port_range` به `10240 65535` | **پس گرفته شد** | پورت‌هایی که پنل‌ها (x-ui، ۱۰۰۰۰–۳۲۷۶۷) برای inbound می‌گیرند ephemeral می‌شد ← «address in use»؛ ~۲۸هزار پورت پیش‌فرض برای ≤۳۰۰ لینک کافی است | `tune/tune.go:363-366,402-407,545-561` |
| فایل ایستای `/etc/sysctl.d/99-hs2.conf` در نصاب | **حذف شد** | منبع دوم کهنه؛ اکنون تنظیم در باینری | `install.sh:294-307`، `install.sh:4087-4090` |
| سقف ثابت ۳۲ برای همه | جایگزین شد | در ۳۰۰–۴۰۰ کاربر فعال (~۵۰ لینک می‌خواهد) گلوگاه بود | `CHANGELOG.md:445-450` |
| سقف ثابت ۶۴ برای همه | رد شد | VPS یک‌گیگ تا ~۵۱۲MiB بافر می‌گرفت | `CHANGELOG.md:447-449` |
| سقف موقت dgtun (۶۴ روی raw، ۱۲۸ روی udp) | برداشته شد | آزمون بار ۳۰۰ حامل: ۱۷۶ Mbit/s روی سوکت raw مشترک؛ udp با p99 ۱۲۱ms | `linkpool_test.go:494-498`، `CHANGELOG.md:733-737` |
| `TCP_NOTSENT_LOWAT` ≥ ۶۴KiB یا خاموش | رد شد | تأخیر بیشتر روی لینک کند؛ خاموش ۳ تا ۷ برابر بدتر | `tlscarrier/tune_linux.go:12-18` |
| MPTCP پیش‌فرض Go 1.24+ روی شنونده | خاموش شد | `tcp_notsent_lowat` را نادیده می‌گیرد؛ p99 ۸s | `CHANGELOG.md:827-831`، `engine/listen.go:41` |
| صف ارسال عمیق‌تر هنگام اشباع CPU | رد شد | «it moved the number by nothing» — هیچ اثری بر توان نداشت | `CHANGELOG.md:1553-1554` |
| بستن کامل شکاف ~۱۴٪ زیر اشباع CPU با رها کردن نرخ (مثل خاموش کردن قواعد) | رد شد (عمدی) | وقتی CPU آزاد شود بافر مسیر مشترک را پر می‌کند (صدها حذف در شبیه‌ساز) | `CHANGELOG.md:1554-1558` |
| doctor با `modprobe` برای بررسی دسترس‌پذیری | اصلاح شد | doctor باید فقط‌خواندنی و بی‌نیاز به ریشه باشد | `CHANGELOG.md:100-101`، `doctor.go:406-414` |
| شمارش «max_links خود هر تونل» در جمع تونل‌ها | اصلاح شد | روی خارج direct عددی اعمال‌نشده بود | `linkpool_test.go:623-626` |
| نام بردن هسته‌های میزبان کنار سقف | اصلاح شد | زیر سهمیهٔ cgroup «16 cores» کنار سقف ۲هسته‌ای نشان می‌داد | `linkpool_test.go:663-665` |
| شمارندهٔ `pacer_dropped` | حذف شد (مرده) | آزمون تضمین می‌کند دیگر نوشته نشود | `hostcpu_test.go:280-282` |
| «certbot renews automatically» در status | حذف شد | گواهی DNS-01 دستی هرگز خودکار تجدید نمی‌شود | `status.go:540-542` |

---

## ۱۳. محدودیت‌های شناخته‌شده و مشاهده‌ها

> برچسب «مشاهده» = چیزی که در کد دیده شد؛ پیشنهاد تغییر کد نیست.

1. **مشاهده — سیگنال‌های کنترلی مسیر داده به نویسندهٔ وضعیت وابسته‌اند.** `SetTCPMemPressure` و `SetHostSaturated` فقط از `startStatusWriter` صدا زده می‌شوند (`status.go:330,854`). اگر `MkdirAll("/run/hs2")` شکست بخورد، تابع زود برمی‌گردد (`status.go:252-254`) و این دو سازوکار (نگهبان فشار حافظه و اعتبار اشباع pacer) **بی‌صدا غیرفعال** می‌شوند. همچنین هنگامی که `/proc/net/sockstat` خوانا نیست (`readTCPMem` ناموفق)، `check()` قبل از `SetTCPMemPressure` برمی‌گردد (`status.go:842-844`).
2. **مشاهده — حامل‌های تک‌نشستی فایل وضعیت ندارند.** `startStatusWriter` فقط در `runStream` (`main.go:481,516`) و `runDgTun` (`main.go:875`) وصل است؛ برای `udp`/`auto`/`noise`/`reality` در کد تولید فراخوانی‌ای پیدا نشد ← `hs2 status` «no live status yet» و doctor «running: no live status» نشان می‌دهند، و پایش CPU/حافظه هم اجرا نمی‌شود.
3. **مشاهده — l3mtcp: کانال جانبی hs0 در وضعیت دیده نمی‌شود.** هیچ فیلد وضعیتی برای بسته‌ها/حذف‌های hs0 نیست؛ تنها نشانه لاگ ۳۰ثانیه‌ای `l3: dropped …` است (`engine/l3_link.go:380-389`). `checkTun` فقط بالا بودن و آدرس را بررسی می‌کند (نه MTU، نه txqueuelen، نه qdisc).
4. **مشاهده — qdisc کارت شبکهٔ فیزیکی کنترل نمی‌شود.** فقط `net.core.default_qdisc` نوشته می‌شود و هیچ `tc qdisc replace` در کد نیست (جستجو شد). این sysctl فقط بر رابط‌هایی اثر دارد که بعداً ساخته می‌شوند (مثل hs0، که کامنت `tune/tune.go:307-308` هم می‌گوید)؛ رابط اصلی (eth0) qdisc زمان بوت را نگه می‌دارد و چون مقدار پایدار نمی‌شود (فقط `tcp_bbr` در `/etc/modules-load.d/hs2.conf`)، پس از reboot هم با تنظیم توزیع بالا می‌آید. doctor فقط مقدار sysctl را مقایسه می‌کند، پس «match» گزارش می‌دهد حتی اگر qdisc واقعی eth0 فرق داشته باشد. (اینکه توزیع‌های معمول خودشان `fq_codel` می‌گذارند: «نامطمئن»، وابسته به توزیع.)
5. **مشاهده — `hs2 tune` (حتی بدون `--apply`) فقط‌خواندنی نیست:** `buildTunePlan` از `tune.AvailableCC`/`AvailableQdisc` استفاده می‌کند که در صورت نبودن، `modprobe` اجرا می‌کنند (`tune/tune.go:493,509`؛ `main.go:558,583`). doctor عمداً این کار را نمی‌کند.
6. **مشاهده — sysctlها سراسری‌اند ولی بخش `tuning` برای هر تونل جداست.** با چند تونل روی یک سرور، هر کدام هنگام شروع Plan خودش را می‌نویسد؛ آخرین شروع‌شده برنده است و doctor تونل دیگر «kernel tuning … differ» هشدار می‌دهد. `setMemoryLimit` هم برای هر فرایند نصف RAM است (جمع چند تونل می‌تواند از RAM بگذرد؛ `checkTunnelsTogether` فقط بافر لینک را جمع می‌زند، نه حد Go).
7. **مشاهده — ادعای کامنت بدون آزمون:** `tune/tune.go:203-205` می‌گوید «cmd/hs2 tests keep it in step with engine.SmuxSessionBuffer» ولی جستجو در کل مخزن هیچ آزمونی که `LinkWorstCaseMiB` را با `SmuxSessionBuffer` مقایسه کند نشان نداد. علاوه بر این `HS2_TUNE_SMUX_SESSBUF` بافر را در زمان اجرا تغییر می‌دهد بدون آنکه سقف لینک تغییر کند.
8. **مشاهده — قاعدهٔ ۲۵٪ فقط بافر smux فضای کاربر را می‌شمارد.** بافرهای سوکت هسته (تا `tcp_rmem[2]` = ۸/۱۶/۳۲ MiB برای هر سوکت لینک و هزاران سوکت کاربر) جدا هستند و فقط با `tcp_mem` سراسری کرنل محدود می‌شوند؛ hs2 به `tcp_mem` دست نمی‌زند (فقط پایش می‌کند).
9. **مشاهده — آستانه‌ها روی `MemTotal` است، نه RAM اسمی.** نمونه‌ها (اجرا شده): «۲ گیگ» با MemTotal حدود ۱۹۵۰ ← medium/۴۸؛ «۴ گیگ» با MemTotal حدود ۳۸۰۰ و ۲ هسته ← **medium** (بافر ۱۶MiB)، سقف ۷۹؛ تنها با ≥۴۰۹۶ یا ≥۴ هسته high می‌شود. CHANGELOG هم «1.9 GB "medium" boxes» را ذکر کرده (`CHANGELOG.md:529`).
10. **مشاهده — آستانه‌های تازگی ناهمسان:** status/doctor «کهنه» را `>6s` می‌گیرند (`status.go:456`، `doctor.go:131`)، ولی `hs2 ports` و نصاب «تازه» را `≤7s` (`ports.go:256`، `install.sh:2532`). اثر عملی کوچک است (یک ثانیه).
11. **مشاهده — اختلاف cgroup بین daemon و doctor:** doctor `detectHW` را در cgroup پوستهٔ کاربر حساب می‌کند، نه cgroup سرویس. اگر اپراتور روی واحد systemd `MemoryMax`/`CPUQuota` بگذارد، doctor ممکن است «auto: running with X, but a start now would give Y» را به‌اشتباه نشان دهد. واحد نصاب چنین محدودیتی ندارد (`install.sh:832-845`)، پس در نصب پیش‌فرض بروز نمی‌کند.
12. **مشاهده — cpuMeter «سه نمونهٔ پیاپی» را دقیق اجرا نمی‌کند:** نمونه‌های بین ۷۰٪ و ۹۰٪ شمارنده را نه افزایش می‌دهند نه صفر می‌کنند (`status.go:390-402`)، پس سه نمونهٔ داغ با فاصلهٔ نمونه‌های میانی هم لاگ را فعال می‌کند. (hostMeter برخلاف آن، در ناحیهٔ میانی هر دو شمارنده را صفر می‌کند: `hostcpu.go:238-239`.)
13. **مشاهده — doctor و status معیار PSI متفاوت دارند:** پایش زنده `avg10` را با ۴۰/۲۰ می‌سنجد؛ doctor `avg60` را با همان اعداد (`doctor.go:640-642`) — عمدی به نظر می‌رسد (یک‌ثانیه‌ای بودن doctor) ولی مستند نشده.
14. **مشاهده — مستند کهنه:** `README.md:839-840` و `hs2-src/README.md:151-152` هنوز می‌گویند نصاب `/etc/sysctl.d/99-hs2.conf` می‌نویسد، در حالی که `install.sh:299-307` آن را حذف می‌کند و بخش «Automatic kernel tuning» همان README (`README.md:249-261`) درست است.
15. **مشاهده — نام cc/qdisc اعتبارسنجی نمی‌شود:** `check.go:350-366` فقط mode و اعداد را بررسی می‌کند؛ اشتباه تایپی در `tuning.congestion` بی‌صدا به bbr/cubic جایگزین می‌شود (با یادداشت در لاگ).
16. **مشاهده — preset «Custom» در نصاب** backlog و somaxconn را همیشه ۸۱۹۲/۴۰۹۶ می‌گذارد (`install.sh:3565` در `tm_tune_manual`)، صرف‌نظر از اندازهٔ سرور.
17. **مشاهده — اعمال tune فقط یک بار در شروع است:** اگر کسی یا برنامهٔ دیگری sysctlها را بعداً تغییر دهد، hs2 تا ری‌استارت دوباره اعمال نمی‌کند؛ فقط doctor ناهمخوانی را نشان می‌دهد.
18. **محدودیت شناخته‌شده (مستند در کد):** سقف خودکار فقط در شروع حل می‌شود؛ تغییر RAM زیر daemon در حال اجرا تا ری‌استارت اثر ندارد (doctor هشدار/اطلاع می‌دهد، `doctor.go:315-318,374-379`).

---

## ۱۴. ارجاع به زیرسیستم‌های دیگر

**این زیرسیستم صدا می‌زند / می‌خواند:**
- `engine.StatsFn` → `engine.PoolStats` (`engine/linkmanager.go:2216-2290`، `engine/stream_iran.go:51`).
- `engine.SetTCPMemPressure` (`engine/mempressure.go:29`) ← مصرف در `engine/wedge.go:158` و `engine/linkmanager.go:1671-1677`.
- `udpcarrier.SetHostSaturated` (`udpcarrier/pacer.go:130`) ← مصرف در `udpcarrier/pacer.go:307`.
- `encap.SendRefused()` (برای `send_refused`)، `encap.DefaultIPXProto` (doctor).
- `engine.WarmSize` (`engine/health.go:240`)، `engine.DgTunPort/DgTagPort` و `engine.PeerRoutes` (پورت‌ها).
- `tlscarrier.CongestionControl`، `NotSentLowat` (`tlscarrier/tune_linux.go`)، `engine.SmuxFrameSize/StreamBuffer/SessionBuffer` (`engine/mtcp_link.go:254-265`).
- `firstCertExpiryDays`، `watchCerts`، `reloadAllCerts` (`cmd/hs2/cert.go`).
- `checkConfig`، `checkPoolBounds` (`cmd/hs2/check.go`)؛ `checkPorts`، `readLive`، `portLines`، `fillPeerRoutes` (`cmd/hs2/ports.go`).
- `/proc`: `meminfo`، `self/cgroup`، `self/stat`، `stat`، `pressure/cpu`، `net/snmp`، `net/sockstat`، `net/tcp*`، `net/udp*`، `sys/net/...`؛ و `/sys/fs/cgroup`.

**از کجا صدا زده می‌شود / چه کسی می‌خواند:**
- `runCmd` (`main.go:322-425`): `applyTuning`، `setMemoryLimit`، `buildTunePlan`+`Apply`، `ceilingLogLine`.
- `runStream` (`main.go:453-548`) و `runDgTun` (`main.go:829-899`): `linkEnvelope`، `warmLinks`، `OnStart → startStatusWriter`.
- موتور: `RunIran`، `RunKharej`، حلقهٔ reverse، `dgpool` ← هر کدام یک‌بار `OnStart`.
- نصاب `install.sh`: `status_path`/`status_fresh` و خواندن کلیدهای فایل وضعیت (`install.sh:2520-2533`، منوی مانیتور زنده)، `recommend_links` و `use_auto_link_ceiling` (`install.sh:1761-1790`)، `tm_tune`/`tm_tune_manual`/`tm_tune_links`/`tm_live_ceiling`/`tm_doctor`، و حذف فایل‌های `.status.json`/`.warm` هنگام حذف تونل.
- `hs2 ports` (`ports.go:251-257`) از فایل وضعیت نیمهٔ جدول طرف مقابل را می‌خواند.
