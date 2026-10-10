# مسیر L3/TUN روی mtcp (قلب تونل اصلی)

> دامنه: حامل `l3mtcp` (و نام قدیمی `l3`) و هم‌خانوادهٔ تک‌لینکی‌اش `tls`. همهٔ ارجاع‌ها نسبت به `/home/user/hs2-/hs2-src` است، مگر جایی که مسیر کامل آمده. شاخهٔ بررسی‌شده: `main` در ثبت `0812bc9`.
> قرارداد برچسب‌ها: «واقعیت» = مستقیم از کد خوانده شد؛ «نامطمئن» = استنباط یا چیزی که کامل وارسی نشد؛ «مشاهده» = نقطهٔ ضعف احتمالی، بدون پیشنهاد تغییر.

---

## ۰. خلاصهٔ یک‌صفحه‌ای (مهم‌ترین واقعیت‌ها)

1. **در l3mtcp، ترافیک پورت‌های کاربر از TUN نمی‌رود.** اتصال TCP کاربر روی سرور ایران تمام می‌شود و **بایت‌هایش** روی یک جریان smux جدا (`kindTCP`/`kindTCPPort`) به خارج می‌رود؛ خارج خودش به پنل وصل می‌شود (`engine/stream.go:18-31`، `engine/stream_iran.go:260-268`، `engine/stream_kharej.go:199-244`). UDP پورت‌های کاربر هم برای هر نشانی کلاینت یک جریان smux جدا دارد (`stream_iran.go:302-416`).
2. **hs0 فقط «کانال جانبی» است:** روی **هر** لینک mtcp یک جریان smux اضافه با بایت نوع `kindL3 = 3` باز می‌شود و بستهٔ IP خام در قاب‌های ۷ بایتی سرآیند‌دار روی آن می‌رود (`stream.go:29-36`، `stream_iran.go:418-438`، `stream_kharej.go:245-253`). هر چیزی که هستهٔ سیستم‌عامل به hs0 مسیر بدهد (پینگ و ترافیک به 10.77.x، یا هر مسیری که اپراتور خودش روی hs0 بگذارد) از این راه می‌رود. نصب‌کننده صریحاً می‌گوید این برای «ping و ترافیک سبک است، نه حجیم» (`install/install.sh:1442-1443`).
3. **انتخاب لینک برای هر بستهٔ hs0 = هش rendezvous روی جریان** (`l3_link.go:305-323`) با FNV-1a روی (src,dst,proto,sport,dport) برای IPv4 TCP/UDP (`l3_link.go:399-415`) و `mix32` (murmur3 finaliser). بدون جدول چسبندگی (sticky)، بدون آگاهی از سلامت/بار لینک؛ فقط پرچم `dead` دیده می‌شود.
4. **صف هر لینک:** کانال ۲۵۶ بسته‌ای (`l3QueueLen`)، دورریز هنگام پر بودن، و دورریز بسته‌ای که بیش از **۶۰ میلی‌ثانیه** (`l3MaxSojourn`) در صف مانده؛ نویسندهٔ هر لینک تا ~۱۶ کیلوبایت (`l3BatchBytes`) را در یک `WriteRaw` جمع می‌کند (`l3_link.go:49-62, 202-243`).
5. **مرگ لینک L3:** خطای `WriteRaw` (مهلت نوشتن **۵ ثانیه** روی جریان smux، `stream.go:214-218`)، خطای خواندن (مهلت **۳۰ ثانیه** بی‌قاب، `l3StreamDeadAfter`)، یا سکوت کامل نشست به مدت **۱۲ ثانیه** (`l3SessionSilent`، `watchSession`) ⇒ `markDead` (یک‌طرفه و بی‌بازگشت) ⇒ جریان‌هایش با rendezvous به لینک‌های دیگر می‌روند. **جریان L3 روی همان لینک هرگز دوباره باز نمی‌شود** (فقط در `OnLink` یک بار).
6. **در l3mtcp هیچ offload/GSO/GRO، هیچ دسته‌نویسی TUN (`tunBatch`) و هیچ بازچین (`reorderer`) فعال نیست.** hs0 با `tun.Open` بدون `Options.Offload` باز می‌شود (`cmd/hs2/main.go:462`)؛ این سازوکارها فقط در `dgtun` سیم‌کشی شده‌اند (`cmd/hs2/main.go:841`، `engine/dgpool.go:670-671`).
7. **MTU پیش‌فرض باینری ۱۳۸۰** (`main.go:458-461`)، پیش‌فرض نصب‌کننده ۱۳۲۰ (`install.sh:1668-1669`)؛ `txqueuelen 2000` (`tun/tun_linux.go:125`)؛ هیچ MSS clamp، هیچ NAT/MASQUERADE و هیچ `ip_forward` توسط hs2 تنظیم نمی‌شود (جست‌وجوی کامل کد Go و `install.sh`).
8. **قالب روی سیم (از بالا به پایین):** `[ftype:1][len:3][pad:3=0][IP packet]` ← سرآیند ۸ بایتی smux v2 (حداکثر قاب ۱۶ KiB) ← قاب شکل‌دهندهٔ طول `[dataLen u16][padLen u16][data][pad]` با اندازه‌های شبه‌HTTPS ۴۰ تا ۱۴۰۰ بایت ← رکورد TLS 1.3 ← TCP با BBR، `TCP_NOTSENT_LOWAT=32KiB`، `TCP_USER_TIMEOUT=20s`.

---

## ۱. نقش و جایگاه در کل سیستم

### ۱.۱ چه چیزی l3mtcp را می‌سازد
- `cmd/hs2/main.go:405-424`: `case "l3mtcp", "l3": runStream(ctx, fc, true, 0)` (خط 410-411). یعنی همان «هستهٔ جریانی» (`mtcp`) به‌علاوهٔ `withTUN=true`. حامل `tls` همان مسیر با `links=1` است (`main.go:412-413`)؛ `mtcp` همان مسیر بدون TUN (`main.go:408-409`).
- `runStream` (`main.go:446-551`):
  - اگر `withTUN`: `mtu := fc.MTU`، اگر صفر ⇒ `1380` (`main.go:458-461`)؛ `tun.Open(fc.Iface, fc.LocalCIDR, fc.PeerIP, mtu)` (`main.go:462`)؛ لاگ `tun %s up: %s peer %s (side channel)` (`main.go:466`).
  - نقش با `fc.Mode` تعیین می‌شود: `"dial"` = لبه (ایران) (`main.go:468`)؛ `fc.Reverse` فقط جهت شمارهٔ‌گیری TLS را عوض می‌کند نه نقش را.
  - لبه: `engine.RunIran(ctx, IranConfig{..., TUN: dev})` (`main.go:470-500`)؛ خروجی: `engine.RunKharej(ctx, KharejConfig{..., TUN: dev})` (`main.go:502-551`).
- دستگاه TUN **یک بار برای کل عمر فرایند** باز می‌شود و با مرگ حامل‌ها/نشست‌ها پایین نمی‌آید (توضیح بسته در `tun/tun_linux.go:1-5`).

### ۱.۲ سهم TUN در برابر جریان‌های پراکسی TCP (دقیق)

| نوع ترافیک | از کجا وارد می‌شود | چطور منتقل می‌شود | کجا خارج می‌شود |
|---|---|---|---|
| TCP کاربر روی `forward_ports` ایران | `ListenReuse` روی پورت کاربر (`stream_iran.go:138-161`) | یک جریان smux با سرآیند `kindTCP` یا `[kindTCPPort][port u16]` (`stream_iran.go:212-258`)، relay بایتی (`wedge.go:295`) | خروجی `net.DialTimeout("tcp", target, 5s)` به پنل/هدف `port_map` (`stream_kharej.go:199-244`) |
| UDP کاربر روی همان پورت‌ها (اگر `"udp": true`) | `net.ListenPacket` (`stream_iran.go:163-170`) | یک جریان smux برای هر نشانی کلاینت، دیتاگرام‌ها `[len u16][payload]` (`stream.go:243-267`) | خروجی `net.Dial("udp", target)` (`stream_kharej.go:222-231`) |
| هر بستهٔ IP که هسته به hs0 بفرستد (پینگ 10.77.0.2، TCP/UDP به آی‌پی تونل، یا مسیرهایی که اپراتور خودش روی hs0 گذاشته) | `pumpTun` از `dev.Read` (`l3_link.go:334-357`) | قاب `TypeData` روی جریان `kindL3` لینک برگزیدهٔ rendezvous | `dev.Write` روی hs0 سمت دیگر (`l3_link.go:361-377`) |
| کنترل، آمار، اطلاعات همتا، کنترل استخر (وارونه) | داخلی | جریان‌های خام `kindCtrl`/`kindStats`/`kindInfo`/`kindPool` | داخلی |

- توضیح کد: «The TUN (hs0), when a mode has one, is a side channel for non-TCP traffic (ping, UDP to 10.77.x) carried as packets on a dedicated stream per link.» (`stream.go:29-31`).
- README (`/home/user/hs2-/README.md:659-665`): «`tls` and `l3mtcp` used to carry IP packets (TCP inside TCP) and built multi-second queues under load; they now use the stream core. The tun interface remains in those modes as a **side channel** for ping and light non-TCP traffic, with a 60 ms queue-time limit — it is not a bulk path».
- حالت «تونل مسیریابی‌شدهٔ خالص»: اگر اپراتور در نصب، پورت کاربری ندهد (`install.sh:2123`)، هشدار می‌گیرد که کاربران فقط وقتی به پنل می‌رسند که «خودش» ترافیک را به سمت آی‌پی تونل خارج مسیر بدهد (`install.sh:2144, 2289`). در آن حالت ترافیک حجیم کاربر از کانال جانبی (TCP-درون-TCP) می‌گذرد. **نامطمئن:** این‌که کاربر این پروژه l3mtcp را با `forward_ports` یا به‌صورت مسیریابی‌شده (مثلاً DNAT به 10.77.0.2) به کار می‌برد از کد معلوم نیست.
- آزمون انتها‌به‌انتها همین هم‌زیستی را تضمین می‌کند: `TestTunModeReverseWithUserPorts` (`engine/tun_mode_test.go:198-237`): «user TCP ports carried on the multi-link stream path (no TCP inside TCP), and at the same time the hs0 L3 interface for everything else (ping, UDP, other ports routed via 10.77.0.x). Both must work over the same links.»

### ۱.۳ نشانی‌ها و مسیرها
- هر تونل یک ‎/30 درون `10.77.0.0/16`: ایران = base+1، خارج = base+2 (`install.sh:37-47, 604-609`). پیش‌فرض قدیمی `10.77.0.1/30` و `10.77.0.2/30`.
- `OpenWith` فقط این‌ها را اجرا می‌کند (`tun/tun_linux.go:122-136`): `ip addr replace <local_cidr> dev <if>`، `ip link set dev <if> mtu <mtu>`، `ip link set dev <if> txqueuelen 2000`، `ip link set dev <if> up`، و اگر `peer_ip` باشد `ip route replace <peer>/32 dev <if>`. پیش از همه `ip link del <name>` برای پاک کردن رابط کهنهٔ هم‌نام (`tun_linux.go:100-102`).
- **هیچ** قاعدهٔ NAT، MASQUERADE، DNAT، MSS clamp یا `net.ipv4.ip_forward` در کد Go یا `install.sh` برای hs0 نیست (جست‌وجوی `ip_forward|MASQUERADE|TCPMSS|clamp` فقط موارد بی‌ربط آورد). تنها sysctlهای سراسری مرتبط که hs2 می‌نشاند: `rp_filter=2` برای `all` و `default` (`tune/tune.go:367-370`)، `tcp_mtu_probing=1` (`tune/tune.go:354`)، و `default_qdisc` (پیش‌فرض `fq_codel`، `tune/tune.go:300-309, 341-342`). **نامطمئن:** این‌که hs0 در عمل کدام qdisc را می‌گیرد (چون tuning پیش از باز کردن TUN اعمال می‌شود `main.go:352-354`، احتمالاً همان `default_qdisc`).

---

## ۲. اجزای اصلی

### ۲.۱ نوع‌ها

| نام | محل | نقش |
|---|---|---|
| `tunWriter` (رابط) | `engine/l3_link.go:16-20` | آنچه مسیر L3 از TUN می‌خواهد: `Write`, `Read`, `MTU`. `*tun.Device` آن را برآورده می‌کند. |
| `pktConn` (رابط) | `l3_link.go:24-29` | آنچه لینک L3 رویش قاب می‌فرستد: `WriteRaw`, `ReadFrameReuse`, `SetReadDeadline`, `Close`. در تولید فقط `*streamPkt`. |
| `l3Link` | `l3_link.go:90-102` | یک لینک L3: `car pktConn`، `id uint32` (بذر rendezvous، تصادفی)، `q chan qpkt` (صف)، `ka`، `kaFn`، `deadAfter`، `dead atomic.Bool`، `once`، `done chan`. |
| `qpkt` | `l3_link.go:105-108` | بستهٔ صف‌شده + زمان صف شدن (`t`) برای قاعدهٔ ۶۰ ms. |
| `l3Set` | `l3_link.go:247-252` | مجموعهٔ لینک‌های زنده (`links []*l3Link` زیر `RWMutex`)، `pool sync.Pool` بافرهای بسته، `drops atomic.Uint64`. **یک نمونه برای کل فرایند** در هر سمت (`stream_iran.go:90-95`، `stream_kharej.go:87-92`). |
| `streamPkt` | `engine/stream.go:206-241` | پیاده‌سازی `pktConn` روی یک `*smux.Stream`: سرآیند ۷ بایتی، بافر بازمصرف `rbuf`. |
| `tun.Device` | `tun/tun_linux.go:26-47` | دستگاه TUN؛ در l3mtcp با `offload=false`. |
| `tunBatch` | `engine/tunbatch.go:11-53` | دسته‌نویسی TUN — **فقط dgtun** (`dgpool.go:670`). |
| `reorderer` | `engine/reorder.go:91-106` | بازچین TCP سمت دریافت — **فقط dgtun** (`dgpool.go:671`). |
| `RouteTable` | `engine/routes.go:31-46` | جدول «پورت کاربر ⇐ هدف» روی خروجی برای جریان‌های TCP/UDP کاربر. **ربطی به مسیریابی IP روی hs0 ندارد.** |

### ۲.۲ توابع کلیدی

| تابع | محل | کار |
|---|---|---|
| `newL3Link` | `l3_link.go:110-118` | `id: rand.Uint32()`، صف ۲۵۶، `ka = 2s`. در تولید فقط از درون `newStreamL3Link` صدا زده می‌شود (مسیر «یک حامل TLS = یک لینک L3» فقط در آزمون‌ها مانده). |
| `newStreamL3Link` | `l3_link.go:125-138` | سازندهٔ تولید: `deadAfter = 30s`، `kaFn` = اگر همتا `capL3Quiet` گفته ⇒ `jitterAround(10s)` (۸ تا ۱۲ ثانیه) وگرنه ۲ ثانیه؛ اگر شمارندهٔ خواندن نشست داده شده ⇒ `go l.watchSession(...)`. |
| `watchSession` | `l3_link.go:143-161` | هر `min(2s, l3SessionSilent/4)` = ۲ ثانیه شمارندهٔ `rdCalls` نشست را نگاه می‌کند؛ اگر ≥۱۲ ثانیه تکان نخورد ⇒ `markDead`. |
| `sessReadsOf` | `l3_link.go:164-169` | `m.guard.w.rdCalls.Load` (شمارندهٔ فراخوانی‌های `Read` در `watchConn`، `stream.go:106-115`). |
| `keepalive` | `l3_link.go:172-177` | فاصلهٔ بی‌کاری تا keepalive بعدی (هر بار از نو محاسبه می‌شود). |
| `markDead` | `l3_link.go:183-189` | با `sync.Once`: `dead=true`، `close(done)`، `car.Close()` (یعنی `smux.Stream.Close` ⇒ FIN به همتا). |
| `enqueue` | `l3_link.go:192-199` | `select` غیرمسدود روی کانال؛ پر بود ⇒ `false`. |
| `writeLoop` | `l3_link.go:202-243` | تنها نویسندهٔ حامل؛ تخلیه و جمع کردن، دورریز کهنه، keepalive، `WriteRaw`. |
| `l3Set.add/remove` | `l3_link.go:254-270` | افزودن/برداشتن زیر قفل. |
| `l3Set.removeDead/count/closeAll` | `l3_link.go:273-303` | **در کد تولیدی صدا زده نمی‌شوند** (جست‌وجوی کامل). |
| `l3Set.pick` | `l3_link.go:308-323` | rendezvous: برای هر لینک زنده `mix32(flowHash ^ l.id)`؛ بیشینه برنده. |
| `l3Set.serveLink` | `l3_link.go:327-330` | `go writeLoop` و سپس `linkToTun` روی همان goroutine تا مرگ لینک. |
| `l3Set.pumpTun` | `l3_link.go:334-357` | تنها خوانندهٔ TUN؛ هرگز روی لینک مسدود نمی‌شود. |
| `l3Set.linkToTun` | `l3_link.go:361-377` | خوانندهٔ قاب‌ها و نویسندهٔ مستقیم در TUN. |
| `l3Set.logDrops` | `l3_link.go:380-393` | هر ۳۰ ثانیه شمارندهٔ دورریز را صفر و گزارش می‌کند. |
| `flowHash` | `l3_link.go:399-415` | کلید جریان + FNV-1a درون‌خطی. |
| `mix32` | `l3_link.go:431-438` | finaliser مورمور۳. |
| `openL3` | `stream_iran.go:419-438` | لبه: روی لینک تازه `OpenRawStream`، نوشتن بایت `kindL3`، ساخت لینک، `set.add`، `go serveLink; remove`. |
| `serveStream` (شاخهٔ `kindL3`) | `stream_kharej.go:245-253` | خروجی: اگر TUN ندارد ⇒ `st.Close()`؛ وگرنه ساخت لینک، `add`، `serveLink` (هم‌گام)، `remove`. |
| `streamPkt.WriteRaw` | `stream.go:214-218` | `SetWriteDeadline(now+5s)` و `st.Write(b)`. |
| `streamPkt.ReadFrameReuse` | `stream.go:220-238` | خواندن سرآیند ۷ بایتی، رد `n>65536` یا `pad>65536` با خطای `engine: oversized L3 frame`، خواندن بدنه در بافر بازمصرف. |
| `tlscarrier.AppendFrame` | `tlscarrier/carrier.go:34-38` | ساخت قاب `[ftype][len 3B][0,0,0][payload]`. |
| `tun.OpenWith` | `tun/tun_linux.go:99-138` | ساخت رابط و دستورهای `ip`. |
| `tun.openFD` | `tun/tun_linux.go:73-96` | `/dev/net/tun`، `TUNSETIFF` با `IFF_TUN|IFF_NO_PI` (و در صورت offload `IFF_VNET_HDR` + `TUNSETOFFLOAD`). |
| `Device.Read/Write` | `tun_linux.go:155-158, 227-230` | بدون offload: مستقیماً `f.Read`/`f.Write` — یک فراخوان سیستمی برای هر بسته. |

### ۲.۳ goroutineها (برای هر سمت)
- ۱× `pumpTun` برای کل فرایند (`stream_iran.go:93`، `stream_kharej.go:90`).
- ۱× `logDrops` برای کل فرایند (`stream_iran.go:94`، `stream_kharej.go:91`).
- برای **هر لینک** mtcp که L3 دارد: ۱× `writeLoop` (`l3_link.go:328`)، ۱× خوانندهٔ `linkToTun` (لبه: goroutine ساخته‌شده در `stream_iran.go:434-437`؛ خروجی: goroutine `serveStream` خود جریان)، ۱× `watchSession` (`l3_link.go:127-129`). یعنی سه goroutine برای هر لینک در هر سمت؛ در سقف ۳۰۰ لینک ⇒ ۹۰۰ goroutine.
- زیر آن‌ها، goroutineهای خود smux برای هر نشست: `shaperLoop`, `recvLoop`, `sendLoop`, `keepalive` (`/root/go/pkg/mod/github.com/xtaci/smux@v1.5.24/session.go:112-117`).

---

## ۳. جریان داده و کنترل، گام‌به‌گام

### ۳.۱ برپایی کانال جانبی روی یک لینک تازه
1. لبه یک لینک mtcp تازه می‌گیرد: شماره‌گیری (`linkmanager.go:925-948`) یا پذیرش وارونه (`AddLink`, `linkmanager.go:407-438`). هر دو `go m.OnLink(l)` را صدا می‌زنند (`linkmanager.go:434-435, 946-947`). **لینک‌های spare/retiring هم OnLink و در نتیجه L3 می‌گیرند.**
2. `OnLink` (`stream_iran.go:101-129`) جدا از کنترل/آمار/اطلاعات، اگر `l3 != nil`، `openL3` را **هم‌گام** صدا می‌زند (`stream_iran.go:126-128`). L3 منتظر پایان تبادل `kindInfo` نمی‌ماند (دروازهٔ `gateInfo` فقط برای اتصال‌های کاربر است، `stream_iran.go:78-80`).
3. `openL3`: `ro.OpenRawStream()` (`mtcp_link.go:183` — جریانی که بار کاربر شمرده نمی‌شود، `linkmanager.go:212-216`)، نوشتن بایت `kindL3`، `newStreamL3Link(st, peerL3Quiet(meter), sessReadsOf(meter))`، `set.add(pl)`، و `go { set.serveLink; set.remove }` (`stream_iran.go:419-438`). اگر هر گام شکست بخورد، بی‌صدا برمی‌گردد (بدون لاگ و بدون تلاش دوباره).
4. خروجی: `sess.AcceptStream()` ⇒ `go serveStream(...)` (مستقیم `stream_kharej.go:144-151`، وارونه `stream_reverse.go:168-175`). `serveStream` بایت نوع را با مهلت `kindTimeout = 10s` می‌خواند (`stream_kharej.go:174-180`، `stream.go:51`). برای `kindL3`: اگر `l3 == nil` (خروجی بدون TUN) ⇒ `st.Close()`؛ وگرنه `newStreamL3Link(st, peerL3Quiet(mtr), sessReadsOf(mtr))` و `add` و `serveLink` و `remove` (`stream_kharej.go:245-253`).
5. هر دو سمت به‌محض دیدن `capL3Quiet` در `peerInfo` همتا (تبادل `kindInfo`)، فاصلهٔ keepalive را از ۲ ثانیه به ۸–۱۲ ثانیه می‌برند؛ تا پیش از رسیدن اطلاعات، ۲ ثانیه است (`l3_link.go:131-136`، `peerinfo.go:74-80`).

### ۳.۲ مسیر بسته: برنامه/هسته ← hs0 (فرستنده) ← سیم ← hs0 (گیرنده)
1. **هسته ⇐ TUN:** برنامه‌ای بسته‌ای به مقصدی که مسیرش hs0 است می‌فرستد (مثلاً `ping 10.77.0.2`). هسته بسته را در صف txqueue رابط (۲۰۰۰) و سپس روی fd دستگاه TUN می‌گذارد.
2. **`pumpTun`** (`l3_link.go:334-357`): بافری به اندازهٔ `MTU+128` از `sync.Pool` (یا تازه) می‌گیرد، `dev.Read` (یک فراخوان سیستمی برای یک بسته؛ بدون offload `tun_linux.go:156-158`). خطای خواندن ⇒ بافر به pool، و اگر ctx تمام نشده `continue` (بدون مکث).
3. **انتخاب لینک:** `s.pick(pkt)` (`l3_link.go:308-323`): `h := flowHash(pkt)` ⇒ زیر `RLock` روی همهٔ لینک‌ها: رد کردن `!Alive()`، وزن `mix32(h ^ l.id)`، بیشینه. نبود لینک ⇒ `nil`.
4. **صف کردن:** `l.enqueue(bp)` با `select` غیرمسدود؛ `qpkt{b, time.Now()}`. نبود لینک یا صف پر ⇒ بافر به pool و `drops++` (`l3_link.go:352-355`). **pumpTun هرگز روی لینک نمی‌ایستد** (تضمین آزمون `TestPumpNeverBlocksOnStuckLink`).
5. **`writeLoop`** آن لینک (`l3_link.go:202-243`):
   - منتظر یکی از: `done`، بسته در `q`، یا زمان‌سنج بی‌کاری.
   - بستهٔ اول ⇒ `now := time.Now()`؛ `add(p, now)`: اگر `now - p.t > 60ms` ⇒ `drops++` (دور ریخته)، وگرنه `AppendFrame(batch, TypeData, payload)`؛ در هر حال بافر به pool برمی‌گردد (چون کپی شده).
   - **تخلیه:** تا وقتی `len(batch) < 16 KiB` و صف بستهٔ آماده دارد، بقیه را هم با همان `now` اضافه می‌کند (`l3_link.go:222-230`). پس دسته می‌تواند تا یک قاب بیش از ۱۶ KiB شود (ظرفیت اولیه `16KiB+4096`، `l3_link.go:203`).
   - اگر همه کهنه بودند ⇒ چیزی نمی‌فرستد و به حلقه برمی‌گردد (`l3_link.go:231-233`)؛ زمان‌سنج بی‌کاری هم Reset نمی‌شود.
   - زمان‌سنج بی‌کاری ⇒ قاب `TypePing` با بار صفرِ تصادفی به طول `0..95` (`core.KeepalivePad`, `core/shape.go:96-100`).
   - `l.car.WriteRaw(batch)`؛ خطا ⇒ `markDead` و پایان. موفقیت ⇒ `idle.Reset(l.keepalive())`.
6. **`streamPkt.WriteRaw`** (`stream.go:214-218`): `SetWriteDeadline(now+5s)` و `smux.Stream.Write(b)`. در smux v2 (`smux@v1.5.24/stream.go:339-420`): اگر پنجرهٔ همتا جا دارد، داده به قاب‌های ≤ `MaxFrameSize` (= `SmuxFrameSize` = ۱۶ KiB) خرد می‌شود و هر قاب از `writeFrameInternal` (`session.go:530-557`) به heap زمان‌بند smux می‌رود و **تا نوشته شدن همان قاب صبر می‌کند**؛ اگر پنجره پر است تا `chUpdate` یا مهلت صبر می‌کند. نتیجه: یک دستهٔ ۱۶ KiB = ۱ یا ۲ قاب smux.
7. **زمان‌بند smux** (`smux@v1.5.24/shaper.go:9-15`, `session.go:425-...`): heap مرتب بر اساس `class` (کنترل قبل از داده) و سپس `seq` سراسری درخواست ⇒ میان جریان‌های داده عملاً FIFO؛ چون هر جریان در هر لحظه فقط یک قاب در راه دارد، جریان‌ها «قاب به قاب نوبت می‌گیرند» (توضیح `mtcp_link.go:255-257`). **جریان L3 هیچ اولویتی بر جریان‌های کاربر روی همان لینک ندارد.**
8. **`sendLoop` smux** (`session.go:470-505`): سرآیند ۸ بایتی `[ver][cmd][len u16 LE][sid u32 LE]` + داده را با **یک** `conn.Write` می‌نویسد (چون پوشش‌ها `WriteBuffers` ندارند).
9. **زنجیرهٔ conn** (`stream.go:172-203`): `watchConn` (ثبت خطا و `rdCalls`) ← `meteredConn` (شمارش بایت/انسداد برای سلامت، `health.go:170-196`) ← `shapedConn` (`shape.go:60-100`) ← اتصال TLS.
10. **`shapedConn.Write`:** برای هر تکه اندازهٔ هدف از `obfs.NewHTTPSLengthSampler` نمونه می‌گیرد (اندازه‌ها و وزن‌ها: 1400:0.55، 1200:0.08، 900:0.05، 600:0.05، 400:0.05، 250:0.06، 150:0.06، 80:0.06، 40:0.04 — `obfs/shaper.go:35-56`)؛ داده را تا `target-4` می‌بُرد، تکهٔ آخر/کوچک را تا هدف **پد** می‌کند، و هر قاب `[dataLen u16][padLen u16][data][pad]` را با یک `Write` روی TLS می‌نویسد ⇒ هر قاب = یک رکورد TLS.
11. **TLS/TCP:** سمت شماره‌گیر uTLS با اثرانگشت `HelloChrome_133` (`tlscarrier/carrier.go:192`)، سمت سرور `crypto/tls` (`tlscarrier/server.go:72-...`)؛ `tuneTCP` در هر دو سمت (`carrier.go:190`، `server.go:74`): `NoDelay`، `TCP_NOTSENT_LOWAT = 32 KiB`، `TCP_USER_TIMEOUT = 20000ms`، `TCP_CONGESTION = bbr` (`tlscarrier/tune_linux.go:11-51`). سمت شماره‌گیر TCP keepalive با دورهٔ ۳ ثانیه (`carrier.go:186-189`).
12. **گیرنده:** `recvLoop` smux داده را در بافر جریان L3 (پنجرهٔ هر جریان `SmuxStreamBuffer = 2 MiB`، کل نشست `SmuxSessionBuffer = 8 MiB`، `mtcp_link.go:258-264`) می‌گذارد.
13. **`linkToTun`** (`l3_link.go:361-377`): پیش از هر قاب `SetReadDeadline(now + deadAfter)` (حالت جریانی ۳۰ ثانیه)؛ `ReadFrameReuse`؛ خطا ⇒ بازگشت و `defer markDead`. قاب `TypeData` ⇒ `dev.Write(payload)` **هم‌گام و بدون بررسی خطا**؛ هر نوع دیگر (Ping) فقط نشانهٔ زنده بودن است و مهلت را تمدید می‌کند. پاسخ Pong وجود ندارد.
14. **TUN ⇐ هسته:** `Device.Write` بدون offload = `f.Write(p)` (`tun_linux.go:227-230`)؛ هسته بسته را مثل بستهٔ رسیده از رابط hs0 پردازش می‌کند (تحویل محلی برای 10.77.x؛ ارسال به جای دیگر فقط اگر اپراتور forward/NAT تنظیم کرده باشد).

### ۳.۳ جهت برگشت
دقیقاً همان سازوکار، با `l3Set` سمت دیگر و `id`های تصادفی **مستقل** آن سمت. چون `flowHash` جهت‌دار است (src/dst و sport/dport جابه‌جا می‌شوند، `l3_link.go:404-408`) و `id`ها در دو سمت مستقل‌اند، رفت و برگشت یک جریان معمولاً روی **دو لینک متفاوت** می‌افتند (واقعیت از کد؛ اثر عملی‌اش بررسی نشده).

### ۳.۴ مقایسه با مسیر TCP کاربر (برای روشن شدن مرز)
- `serveUserTCP` ⇒ `openStream` ⇒ `pickWait(ctx, lm, hold=true)` (تا ۴۰ بار × ۱۵۰ ms ≈ ۶ ثانیه صبر برای لینک، به‌علاوهٔ «refill hold»، `stream_iran.go:183-210`) ⇒ `LinkManager.Pick` (آگاه از سلامت: `retiring`/`degraded`/`draining`/`suspect` کنار گذاشته می‌شوند، `linkmanager.go:300`) ⇒ `OpenStream` + سرآیند ⇒ `relayStream` با نگهبان wedge.
- مسیر L3 هیچ‌کدام از این‌ها را ندارد: نه انتظار برای لینک، نه آگاهی از سلامت، نه نگهبان wedge؛ فقط rendezvous روی لینک‌های غیرمرده.

### ۳.۵ وقتی یک لینک می‌میرد
- **مسیرهای رسیدن به `markDead`:**
  1. `WriteRaw` خطا داد (مهلت ۵ ثانیه، بسته شدن جریان/نشست، خطای سوکت) — `l3_link.go:237-240`.
  2. `linkToTun` خطا گرفت (مهلت خواندن ۳۰ ثانیه، EOF چون همتا جریان را بست، `oversized L3 frame`، بسته شدن نشست) — `l3_link.go:362, 369-372`.
  3. `watchSession`: نشست ≥ ۱۲ ثانیه هیچ `Read` تکمیل‌شده‌ای نداشت — `l3_link.go:153-157`.
  4. پایان ctx: `linkToTun` حلقه را ترک و `markDead` می‌کند.
- **پیامد:** `dead=true` ⇒ `pick` آن را رد می‌کند ⇒ جریان‌هایی که روی آن بودند به لینک بعدیِ بیشینه‌وزن می‌روند؛ جریان‌های بقیهٔ لینک‌ها جابه‌جا نمی‌شوند (`TestPickMovesOnlyDeadLinksFlows`). `close(done)` نویسنده را متوقف می‌کند؛ **بسته‌های مانده در صف همان‌جا رها می‌شوند** (نه ارسال، نه بازگشت به pool). `car.Close()` جریان smux را می‌بندد ⇒ خوانندهٔ همتا EOF می‌گیرد و آن هم `markDead` می‌کند (مرگ قرینه).
- **بازیابی:** هیچ. `openL3` فقط از `OnLink` صدا زده می‌شود (جست‌وجوی کامل)؛ پس اگر لینک mtcp زنده بماند ولی جریان L3‌اش مرده باشد، تا پایان عمر آن لینک hs0 رویش سواری ندارد. لینک‌های تازه (رشد استخر، جایگزینی) دوباره L3 می‌آورند.
- **برداشتن از مجموعه:** پس از برگشتن `serveLink` (یعنی پس از مرگ خواننده)، `set.remove(pl)` (`stream_iran.go:435-436`، `stream_kharej.go:253`).

---

## ۴. جدول ثابت‌ها، آستانه‌ها، بافرها و زمان‌سنج‌ها

| نام | مقدار | محل | معنی |
|---|---|---|---|
| `l3QueueLen` | 256 بسته | `engine/l3_link.go:53` | ظرفیت صف هر لینک L3؛ «عمداً کوتاه» برای جلوگیری از bufferbloat. |
| `l3BatchBytes` | 16 KiB | `l3_link.go:56` | سقف تقریبی یک نوشتن دسته‌ای (حدود یک رکورد کامل TLS در طرح قدیم). بافر دسته `16KiB+4096` (`l3_link.go:203`). |
| `l3MaxSojourn` | 60 ms | `l3_link.go:62` | بسته‌ای که بیش از این در صف مانده دور ریخته می‌شود. |
| `l3KeepaliveEvery` | 2 s | `l3_link.go:67` | keepalive بی‌کاری پیش‌فرض و همچنین وقتی همتا `capL3Quiet` ندارد. |
| `l3DeadAfter` | 8 s | `l3_link.go:68` | مهلت خواندن لینک غیرجریانی؛ **در تولید استفاده نمی‌شود** (همهٔ لینک‌های تولیدی `deadAfter=30s` دارند). |
| `l3StreamDeadAfter` | 30 s | `l3_link.go:78` | مهلت خواندن قاب روی جریان L3 در حالت‌های جریانی. |
| `l3QuietKeepalive` | 10 s (±20% با `jitterAround` ⇒ 8–12 s) | `l3_link.go:79`، `exit_pool.go:565-567` | keepalive کم‌صدا وقتی همتا `capL3Quiet` گفته. |
| `l3SessionSilent` (var) | 12 s | `l3_link.go:87` | سکوت کامل نشست ⇒ رهاکردن کانال جانبی. |
| تیک `watchSession` | `min(2s, 12s/4)` = 2 s | `l3_link.go:144` | پس رهاسازی بین ۱۲ تا ~۱۴ ثانیه پس از آخرین خواندن رخ می‌دهد. |
| دورهٔ `logDrops` | 30 s | `l3_link.go:381` | فاصلهٔ گزارش دورریزها. |
| اندازهٔ بافر خواندن TUN | `MTU + 128` | `l3_link.go:335` | بافر هر بسته از pool. |
| مهلت نوشتن `streamPkt` | 5 s | `engine/stream.go:215` | یک `WriteRaw` که ۵ ثانیه تمام نشود ⇒ خطا ⇒ `markDead`. |
| سقف قاب L3 | `n ≤ 65536`، `pad ≤ 65536` | `stream.go:227-229` | بیشتر ⇒ `engine: oversized L3 frame`. |
| سرآیند قاب L3 | 7 بایت | `stream.go:208`، `tlscarrier/carrier.go:36` | `[ftype][len:3][pad:3]`. |
| `kindL3` | 3 | `stream.go:36` | بایت نوع جریان کانال جانبی. |
| `kindTimeout` | 10 s | `stream.go:51` | مهلت خروجی برای خواندن بایت نوع. |
| `core.TypeData` / `TypePing` | 1 / 3 | `core/frame.go:33, 35` | انواع قاب استفاده‌شده در L3. |
| `core.KeepalivePad()` | تصادفی `0..95` بایت | `core/shape.go:100` | طول بار keepalive. |
| `capL3Quiet` | بیت ۱ (`1<<1`) در `caps` | `engine/peerinfo.go:55` | «خوانندهٔ L3 من ۳۰ ثانیه بی‌قاب را تحمل می‌کند». |
| `infoTimeout` / `infoRetry` / `infoTries` / `infoSlowRetry` | 5s / 10s / 3 / 1min | `peerinfo.go:52, 85-89` | زمان‌بندی تبادل `kindInfo` که حالت keepalive را تعیین می‌کند. |
| `SmuxFrameSize` | 16 KiB | `engine/mtcp_link.go:258` | بزرگ‌ترین قاب دادهٔ smux (`HS2_TUNE_SMUX_FRAME`). |
| `SmuxStreamBuffer` | 2 MiB | `mtcp_link.go:262` | پنجرهٔ دریافت هر جریان (`HS2_TUNE_SMUX_STREAMBUF`). |
| `SmuxSessionBuffer` | 8 MiB | `mtcp_link.go:264` | بافر دریافت کل نشست (`HS2_TUNE_SMUX_SESSBUF`). |
| smux `KeepAliveInterval` | 4–8 s تصادفی برای هر نشست | `mtcp_link.go:278` | NOP smux؛ همان چیزی که `rdCalls` را در لینک بی‌کار تکان می‌دهد. |
| smux `KeepAliveTimeout` | 24 s | `mtcp_link.go:279` | smux نشست بی‌داده را می‌بندد. |
| smux `Version` | 2 | `mtcp_link.go:269` | کنترل جریان هر جریان (`cmdUPD`). |
| سرآیند smux | 8 بایت (`ver,cmd,len u16 LE,sid u32 LE`) | `smux@v1.5.24/frame.go:31-36`، `session.go:486-489` | |
| `shapeHdrLen` / `shapeMaxFrame` | 4 / 32 KiB | `engine/shape.go:46-50` | سرآیند و سقف قاب شکل‌دهنده. |
| اندازه‌های نمونه‌بردار HTTPS | 40…1400 بایت؛ میانگین وزنی ≈ 991 | `obfs/shaper.go:41-42` | هر رکورد TLS لینک (محاسبهٔ میانگین از وزن‌ها، توسط من). |
| `writeTimeout` (حامل TLS خام) | 5 s | `tlscarrier/carrier.go:27` | فقط مسیر غیرجریانی/آزمون‌ها. |
| `NotSentLowat` | 32 KiB | `tlscarrier/tune_linux.go:19` | `TCP_NOTSENT_LOWAT` روی سوکت لینک (`HS2_TUNE_NOTSENT`). |
| `UserTimeoutMs` | 20000 | `tune_linux.go:22` | `TCP_USER_TIMEOUT`. |
| `CongestionControl` | `bbr` (یا انتخاب برنامهٔ tuning) | `tune_linux.go:29`، `main.go:357-359` | |
| TCP keepalive شماره‌گیر | 3 s | `tlscarrier/carrier.go:186-189` | فقط سمت شماره‌گیر TLS. |
| MTU پیش‌فرض باینری (l3mtcp/tls) | 1380 | `cmd/hs2/main.go:458-461` | اگر `mtu` در پیکربندی صفر باشد. |
| MTU پیش‌فرض نصب‌کننده | 1320 | `install/install.sh:1660-1672` | «matches Backhaul»؛ در پیوند `hs2://` حمل می‌شود تا دو سمت یکی باشند. |
| بازهٔ هشدار MTU | `<576` یا `>9000` | `cmd/hs2/check.go:344-345` | فقط هشدار؛ نصب‌کننده 68..65535 را می‌پذیرد. |
| `txqueuelen` | 2000 | `tun/tun_linux.go:125` | |
| `pickWait` (کاربر، نه L3) | 40 × 150 ms | `engine/stream_iran.go:184-208` | برای مقایسه. |
| `suspectAfter` (کاربر، نه L3) | 12 s | `engine/linkmanager.go:317` | هم‌عدد `l3SessionSilent` ولی برگشت‌پذیر. |
| `udpFlowQueue` / `udpFlowBytes` / `udpIdle` | 256 / 512 KiB / 2 min | `stream_iran.go:286-289`، `stream.go:270` | صف UDP کاربر (نه L3). |
| ثابت‌های dgtun (برای مقایسه، در l3mtcp بی‌اثر) | `dgReorderHold=15ms`، `reorderFlowMax=256`، `reorderTotalMax=4096`، `reorderIdle=60s`، `reorderSweep=4096` | `engine/reorder.go:49-62` | |
| ثابت‌های offload (فقط dgtun) | `vnetHdrLen=10`، `minGSOSize=64`، `groMaxSegs=64`، `groMaxBytes=65535` | `tun/offload.go:27, 142, 247-248` | |

حافظهٔ تقریبی (محاسبهٔ من، نه گفتهٔ کد): هر لینک در بدترین حالت `256 × (MTU+128)` بایت بستهٔ صف‌شده (با MTU=1320 حدود ۳۷۰ KB) + بافر دستهٔ ۲۰ KiB + `rbuf` خواننده تا اندازهٔ بزرگ‌ترین قاب.

---

## ۵. حلقه‌های کنترلی

| حلقه | ورودی | شرط/تصمیم | خروجی | دوره |
|---|---|---|---|---|
| `pumpTun` (`l3_link.go:334-357`) | `dev.Read` | `pick` نال یا `enqueue` ناموفق ⇒ دورریز | بسته در صف یک لینک | پیوسته، به‌ازای هر بسته |
| `writeLoop` (`l3_link.go:202-243`) | صف لینک، زمان‌سنج بی‌کاری | سن > ۶۰ ms ⇒ دورریز؛ تا ۱۶ KiB جمع کن؛ بی‌کاری ⇒ Ping | `WriteRaw` | رویدادمحور؛ keepalive هر ۲ s یا ۸–۱۲ s بی‌کاری **نوشتن** |
| `linkToTun` (`l3_link.go:361-377`) | `ReadFrameReuse` | Data ⇒ نوشتن؛ دیگر ⇒ نادیده؛ خطا ⇒ مرگ | `dev.Write` | به‌ازای هر قاب؛ مهلت ۳۰ s برای هر قاب |
| `watchSession` (`l3_link.go:143-161`) | `rdCalls` نشست | بی‌تغییر ≥ ۱۲ s ⇒ `markDead` | مرگ لینک L3 | تیک ۲ s |
| `logDrops` (`l3_link.go:380-393`) | `drops.Swap(0)` | `n > 0` | یک خط لاگ | ۳۰ s |
| keepalive smux (`mtcp_link.go:278`) | — | — | NOP | ۴–۸ s برای هر نشست |
| تشخیص مشکوک در `LinkManager` (`linkmanager.go:1736-1744`) | `rd` متر | بی‌دریافت ≥ ۱۲ s | لینک کاربر نمی‌گیرد (برگشت‌پذیر) | تیک نمونه‌بردار استخر؛ **روی L3 اثری ندارد** |
| `drainTick` (`linkmanager.go:959-...`) | `users`، `Active()` | `users==0 && Active()==0` (+شرط‌های دیگر) ⇒ بستن لینک بازنشسته | بسته شدن نشست ⇒ مرگ L3 آن لینک | — ؛ **ترافیک L3 در شرط نیست** |
| نگهبان wedge (`wedge.go:46-63`) | `rdCalls`/`inRead` | پارک ≥ ۳ نگاه (≥۴ s) ⇒ پایان relayهای گیر | آزاد شدن بافر نشست (L3 هم از آن سود می‌برد) | تیک ۲ s |

---

## ۶. حالت‌ها، گذارها، خطاها و بازیابی

### ۶.۱ ماشین حالت `l3Link`
```
[ساخته‌شده/زنده] --(WriteRaw خطا | خواندن خطا/مهلت 30s/EOF/oversized | سکوت نشست 12s | پایان ctx)--> [مرده]
```
- گذار یک‌طرفه با `sync.Once` (`l3_link.go:183-189`). هیچ حالت «مشکوک» یا «در حال تخلیه» برای L3 نیست.
- `Alive()` = `!dead` (`l3_link.go:179`). `pick` فقط همین را می‌بیند.

### ۶.۲ حالت‌های `l3Set`
- «بدون لینک»: همهٔ بسته‌های TUN دور ریخته و شمرده می‌شوند (`TestPickAllDead`).
- اضافه شدن لینک تازه: بخشی از جریان‌ها (آن‌هایی که وزن لینک تازه برایشان بیشینه است، به‌طور میانگین ~1/(N+1)) **به لینک تازه منتقل می‌شوند** — این از ریاضی rendezvous در `pick` نتیجه می‌شود (واقعیت از کد؛ بر خلاف توضیح «a flow keeps its link for as long as that link lives»، `l3_link.go:306-307`). در dgtun دقیقاً برای همین یک جدول `sticky` افزوده شده است (`engine/dgpool.go:549-556`)؛ در `l3Set` چنین جدولی نیست.

### ۶.۳ خطاهای دستگاه TUN
- باز کردن: `tun: open /dev/net/tun: ... (need root/CAP_NET_ADMIN, tun module)`، `tun: TUNSETIFF ...`، `tun: ip <args>: <err>: <out>` (`tun_linux.go:76, 87, 134`) ⇒ `must(err)` ⇒ پایان فرایند (`main.go:462-463`).
- خواندن: خطا ⇒ `continue` بی‌درنگ (`l3_link.go:344-350`).
- نوشتن: خطای `dev.Write` نادیده گرفته می‌شود (`l3_link.go:374`).

### ۶.۴ ناهمخوانی پیکربندی
- خروجی بدون TUN ولی لبه با TUN: خروجی جریان `kindL3` را می‌بندد (`stream_kharej.go:246-249`) ⇒ لبه EOF ⇒ همهٔ لینک‌های L3 لبه می‌میرند ⇒ همهٔ بسته‌های hs0 دور ریخته و فقط در خط لاگ ۳۰ ثانیه‌ای دیده می‌شوند.
- همتای قدیمی بدون `capL3Quiet`: keepalive سمت ما ۲ ثانیه می‌ماند تا خوانندهٔ ۸ ثانیه‌ای او نمیرد (`l3_link.go:76-77`)؛ همتای کاملاً قدیمی که `kindInfo` را نمی‌شناسد ⇒ `peerInfo` نال ⇒ همیشه ۲ ثانیه.

---

## ۷. قالب پیام‌ها و قاب‌ها

### ۷.۱ پشتهٔ کامل یک بستهٔ hs0 روی سیم
```
IP packet (≤ MTU)                                      ← از hs0
└─ L3 frame:  [ftype=1][len u24 BE][pad u24 = 0][IP packet]           (tlscarrier/carrier.go:34-38)
   (چند قاب پشت‌سرهم در یک WriteRaw تا ~16 KiB)                        (l3_link.go:222-230)
   └─ smux v2 PSH frame: [ver=2][cmd=PSH][len u16 LE][sid u32 LE][≤16 KiB]   (smux frame.go, session.go:486-489)
      (اولین بایت جریان، یک بار: kindL3 = 0x03)                        (stream_iran.go:428)
      └─ shaped frame: [dataLen u16 BE][padLen u16 BE][data][zero pad]  (shape.go:21-25, 60-100)
         اندازهٔ کل از {40,80,150,250,400,600,900,1200,1400}           (obfs/shaper.go:41-42)
         └─ TLS 1.3 record (uTLS Chrome 133 / crypto/tls)
            └─ TCP (BBR, NOTSENT_LOWAT 32 KiB, USER_TIMEOUT 20 s, NoDelay)
```
- keepalive: همان قاب L3 با `ftype=3` و بار صفرِ `0..95` بایتی (`l3_link.go:235`). گیرنده فقط از آن برای تمدید مهلت استفاده می‌کند.
- گیرنده بخش pad قاب L3 را می‌خواند و دور می‌اندازد (`stream.go:226-237`)، هرچند فرستنده همیشه pad=0 می‌گذارد.
- پنجرهٔ smux: قاب‌های `cmdUPD` (۸ بایت: consumed، window) در v2 (`smux frame.go:16-23`).

### ۷.۲ پیام `kindInfo` (بخش مرتبط)
- `[ver][n u8][n bytes]`؛ v2: `maxLinks u16, caps u8, flags u8, count u8, ports...` (`peerinfo.go:22-40`). بیت ۱ `caps` = «quiet L3». هر دو طرف آن را می‌فرستند (`stream_iran.go:81`، `stream_kharej.go:73`). خروجی آن را در `mtr.peerInfo` نگه می‌دارد (`stream_kharej.go:193-198`)؛ `peerL3Quiet(mtr)` همان را می‌خواند (`peerinfo.go:78-80`).

### ۷.۳ سربار تقریبی (محاسبهٔ من)
- برای یک بستهٔ تنها (مثلاً پینگ ۸۴ بایتی): ۸۴ + ۷ (L3) + ۸ (smux) = ۹۹ بایت ⇒ شکل‌دهنده آن را تا اندازهٔ نمونه پد می‌کند: میانگین هدف ≈ ۹۹۱ بایت (۵۵٪ احتمال ۱۴۰۰) + سربار رکورد TLS. یعنی پینگ تنها در حدود ده برابر بایت روی سیم. در دسته‌های پر، پدینگ فقط روی تکهٔ آخر است و سربار ثابت به‌ازای هر ~۱ KB (۴ بایت شکل‌دهنده + سربار رکورد TLS) است.

---

## ۸. متن دقیق لاگ‌های مهم

| متن لاگ | محل | معنی |
|---|---|---|
| `tun %s up: %s peer %s (side channel)` | `cmd/hs2/main.go:466` | hs0 در حالت‌های جریانی (`l3mtcp`/`tls`) باز شد. |
| `l3: dropped %d packets in 30s on the tun side channel (queue limit or no link) — in the TLS modes the tun is for ping and light traffic; the user ports carry the bulk, unaffected` | `engine/l3_link.go:389` | مجموع دورریزهای سه علت: نبود لینک، صف پر، سن بیش از ۶۰ ms. علت‌ها جدا شمرده نمی‌شوند. |
| `tun: open /dev/net/tun: %w (need root/CAP_NET_ADMIN, tun module)` | `tun/tun_linux.go:76` | باز نشدن دستگاه. |
| `tun: TUNSETIFF %s: %v` | `tun_linux.go:87` | ساخت رابط رد شد. |
| `tun: ip %s: %v: %s` | `tun_linux.go:134` | یکی از دستورهای `ip` شکست خورد (مثلاً نبود iproute2). |
| `engine: oversized L3 frame` | `engine/stream.go:228` | خطای برگشتی؛ **لاگ نمی‌شود**، فقط لینک L3 را می‌کشد. |
| `user port %s open (%s), carried over the link pool` | `stream_iran.go:172` | پورت کاربر (مسیر جریانی، نه TUN). |
| `ports: Iran user port %d (%s) has no target on this server — ...` و `ports: a %s connection that does not say its user port ...` | `engine/routes.go:150-154` | جریان کاربر هدف ندارد (حداکثر یک بار در دقیقه برای هر پورت/پروتکل، `routes.go:130`). |
| `link %d: nothing received for %s — not used for new connections until it answers` | `linkmanager.go:1742` | مشکوک شدن لینک برای کاربران؛ L3 جداگانه پس از ۱۲ s بی‌صدا رها می‌شود. |

**نبودن لاگ:** مرگ یک لینک L3، شکست `openL3`، و لینکی که بدون L3 مانده هیچ لاگی ندارند. فایل وضعیت (`hs2 status`) برای l3mtcp هیچ شمارندهٔ TUN ندارد؛ فیلدهای `tun_*` فقط در `runDgTun` پر می‌شوند (`cmd/hs2/main.go:874-881`، `cmd/hs2/status.go:122-138, 312-318`).

---

## ۹. گزینه‌های پیکربندی و متغیرهای محیطی مرتبط

### ۹.۱ کلیدهای پیکربندی (JSON)
- `carrier`: `"l3mtcp"` یا `"l3"` (استخر)، `"tls"` (یک لینک) — `main.go:410-413`.
- `mode`: `"dial"` (لبه/ایران) / `"listen"` (خروجی/خارج)؛ `reverse`: چه کسی TLS را شماره می‌گیرد.
- `iface` (≤ ۱۵ نویسه، `check.go:334-336`)، `local_cidr` (`check.go:337-339`)، `peer_ip` (`check.go:340-342`)، `mtu` (`check.go:344-345`؛ پیش‌فرض باینری ۱۳۸۰).
- `forward_ports`، `udp`، `user_listen_ip` (فقط لبه، مسیر جریانی کاربر).
- `expose`، `port_map` (فقط خروجی، مسیر جریانی کاربر؛ `routes.go`).
- `min_links`/`max_links`/`per_link` ⇒ `linkEnvelope` (`main.go:644-657`؛ پیش‌فرض‌ها ۲ و ۸) — تعداد لینک‌ها = تعداد لینک‌های L3.
- `bind_local_ip`، `sni`، `addr`، `shared_key`، `cert_file`/`key_file`.
- `tuning` (qdisc، cc و …) — `tune/tune.go`.

### ۹.۲ متغیرهای محیطی
| متغیر | اثر در l3mtcp | محل |
|---|---|---|
| `HS2_TUNE_SMUX_FRAME` | اندازهٔ قاب smux | `main.go:268` |
| `HS2_TUNE_SMUX_STREAMBUF` | پنجرهٔ هر جریان | `main.go:269` |
| `HS2_TUNE_SMUX_SESSBUF` | بافر نشست | `main.go:270` |
| `HS2_TUNE_NOTSENT` | `TCP_NOTSENT_LOWAT` | `main.go:267` |
| `HS2_TUNE_CC` | الگوریتم ازدحام سوکت لینک | `main.go:271-274` |
| `HS2_NO_TUNE` | اعمال نکردن sysctlها | `main.go:349-356` |
| `HS2_TUN_REORDER_MS` | **بی‌اثر در l3mtcp** (فقط dgtun) | `engine/reorder.go:49-56` |
| `HS2_TUN_OFFLOAD` | **بی‌اثر در l3mtcp** (فقط dgtun) | `main.go:840` |

هیچ متغیر محیطی یا کلید پیکربندی برای ثابت‌های `l3_link.go` (صف، ۶۰ ms، ۱۶ KiB، keepaliveها، ۱۲ s) وجود ندارد؛ `l3SessionSilent` فقط برای آزمون‌ها `var` است.

---

## ۱۰. آزمون‌ها: چه چیزی تضمین می‌شود

### ۱۰.۱ `engine/l3_link_test.go`
- `TestFlowHashMatchesFNV` (31-40): `flowHash` دقیقاً FNV-1a روی `src+dst`، `proto`، `ports` است.
- `TestPickMovesOnlyDeadLinksFlows` (43-73): با ۸ لینک و ۲۰٬۰۰۰ جریان، هر لینک بین `flows/16` و `flows/4` جریان دارد (توازن)؛ مرگ یک لینک فقط جریان‌های خودش را جابه‌جا می‌کند. (افزودن لینک آزموده نمی‌شود.)
- `TestPickAllDead` (75-83): همه مرده ⇒ `nil`.
- `TestPumpNeverBlocksOnStuckLink` (113-132): لینکی بدون نویسنده، خوانندهٔ TUN را نمی‌ایستاند؛ دورریز دقیقاً `5000 - 256`.
- `TestWriteLoopDeliversAndKeepsAlive` (134-166): ترتیب بسته‌ها حفظ می‌شود و لینک بی‌کار keepalive (`TypePing`) می‌فرستد.
- `TestWriteLoopDropsStalePackets` (168-190): بستهٔ قدیمی‌تر از `l3MaxSojourn` دور ریخته و شمرده می‌شود، تازه فرستاده می‌شود.
- `TestStreamL3QuietKeepaliveNeedsPeerCap` (192-219): `deadAfter=30s`؛ پیش از info و برای همتای بدون `capL3Quiet` ⇒ ۲ s؛ با `capL3Quiet` ⇒ ۸ تا ۱۲ s؛ لینک غیرجریانی دست نخورده.
- `TestStreamL3DroppedWhenSessionSilent` (221-269): وقتی نشست چیزی می‌شنود L3 زنده می‌ماند؛ وقتی ساکت شد ظرف ~`l3SessionSilent` رها می‌شود (اجرای من: «dropped 600ms after the session went silent» با مقدار آزمونی ۶۰۰ ms).

### ۱۰.۲ `engine/tun_mode_test.go`
- `TestTunModeDirect` (155-164) و `TestTunModeReverse` (189-197): لولهٔ L3 کامل روی TLS واقعی با ۳ لینک، هر دو جهت؛ ۶۰ بستهٔ UDP با بار متمایز؛ حداکثر ۵ دورریز مجاز، **هیچ خرابی** مجاز نیست.
- `TestTunModeReverseWithUserPorts` (198-237): هم‌زمانی پورت کاربر (echo ۲۵۶ KiB و ۶۴ KiB) و L3 روی همان لینک‌ها.

### ۱۰.۳ بازچین و دسته‌نویسی (فقط dgtun؛ برای دانستن آنچه هست)
- `engine/reorder_test.go`: `TestReorderInOrderPassThrough`، `TestReorderGapFills`، `TestReorderGapTimesOut`، `TestReorderRetransmitPassesThrough`، `TestReorderPassesNonData`، `TestReorderSeqWrap`، `TestReorderOverflowReleases`، `TestReorderFlowsIndependent`، `TestReorderCloseFlushes`، `TestReorderDisabled`، `TestReorderIPv6`، `TestReorderTimerFires`.
- `engine/tunbatch_test.go`: `TestDgReadLoopBatchesTunWrites` (آنچه با هم رسید در یک `WriteBatch`؛ رهاسازی زمان‌سنج بازچین flush می‌شود)، `TestTunBatchCountsWhatWentIn` (بستهٔ ردشده فقط خودش را از دست می‌دهد).

### ۱۰.۴ `tun/`
- `tun/offload_test.go`: `TestSplitTCP`، `TestCompleteCsum`، `TestCompleteCsumZeroIsFFFF`، `TestCoalesceRoundTrip`، `TestCoalesceRules`، `TestCoalesceRandomBatches`، `FuzzSplitTCP`.
- `tun/tun_linux_test.go`: `TestOffloadDeviceUDP` (در فضای نام شبکهٔ خصوصی با root). در این محیط به دلیل نبودن فرمان `ip` شکست خورد (`exec: "ip": executable file not found in $PATH`) — مشکل محیط، نه کد.
- **هیچ آزمونی** دستگاه TUN بدون offload (مسیر l3mtcp) را در هسته آزمایش نمی‌کند؛ آزمون‌های l3mtcp با `fakeTUN` هستند.

### ۱۰.۵ `engine/routes_test.go` (مسیریابی پورت کاربر، نه IP)
`TestParsePortMap`، `TestRouteTableTarget`، `TestUserStreamHeader`، `TestPickWaitsForInfo`، `TestOpenInfoEndsGate`، `TestPortRoutesEndToEnd`، `TestPortRoutesNoDefaultRefuses`، `TestPortRoutesMixedVersions`، `TestExitInfoForgottenWithNoLinks`.

### ۱۰.۶ آزمون نصب
- `install/tests/tun_ports_test.py`: نصب‌کنندهٔ واقعی برای «tun → tcp» (l3mtcp و tls)، هر چهار شاخه، و در فضای نام: ترافیک واقعی از پورت‌های کاربر به پنل و اثبات با پینگ روی tun (`tun_ports_test.py:1-31`).
- `lab/run.sh:81-84`: در حالت‌های دارای hs0، هنگام بار `ping -i 0.2 10.77.0.2` از راه hs0 اجرا و RTT/اتلاف ثبت می‌شود. **نامطمئن:** این‌که ستون «latency under load» جدول README (`/home/user/hs2-/README.md:682-689`) برای l3mtcp دقیقاً همین پینگ است.

---

## ۱۱. «از قبل وجود دارد» (برای پرهیز از دوباره‌کاری)

**در خود مسیر L3 حالت l3mtcp:**
1. صف محدود جدا برای هر لینک + نویسندهٔ جدا؛ خوانندهٔ TUN هرگز مسدود نمی‌شود (`l3_link.go:34-39, 192-199, 334-357`).
2. دورریز بر اساس زمان ماندن در صف (۶۰ ms) علاوه بر سقف ۲۵۶ بسته (`l3_link.go:57-62, 207-209`).
3. جمع کردن بسته‌های منتظر در یک نوشتن تا ~۱۶ KiB (`l3_link.go:40-42, 222-230`).
4. انتخاب لینک با rendezvous روی هش جریان (FNV-1a + murmur3) (`l3_link.go:305-323, 399-438`).
5. keepalive بی‌کاری با بار تصادفی؛ حالت کم‌صدا (۸–۱۲ s) با مذاکرهٔ `capL3Quiet` در `kindInfo` (`l3_link.go:70-79, 131-136`، `peerinfo.go:31-32`).
6. رهاکردن کانال جانبی وقتی کل نشست ۱۲ s ساکت است (`l3_link.go:82-87, 140-161`).
7. مرگ فوری و قرینه: `markDead` جریان را می‌بندد تا همتا منتظر مهلت نماند (`l3_link.go:181-189`).
8. بافرهای بسته با `sync.Pool`، و `ReadFrameReuse` بدون تخصیص در هر قاب (`l3_link.go:250, 337-341`، `stream.go:220-238`).
9. شمارش و گزارش ۳۰ ثانیه‌ای دورریزها (`l3_link.go:380-393`).
10. نگهداری hs0 در تمام عمر فرایند و پاک کردن رابط کهنهٔ هم‌نام (`tun_linux.go:1-5, 100-102`).

**در لایه‌های زیرین که مسیر L3 از آن سود می‌برد:**
11. شکل‌دهی طول رکوردها شبه‌HTTPS (`shape.go`، `obfs/shaper.go`).
12. BBR، `TCP_NOTSENT_LOWAT=32KiB`، `TCP_USER_TIMEOUT=20s`، `NoDelay` روی هر سوکت لینک (`tlscarrier/tune_linux.go`).
13. smux v2 با پنجرهٔ هر جریان ۲ MiB، نشست ۸ MiB، قاب ۱۶ KiB، keepalive تصادفی ۴–۸ s (`mtcp_link.go:253-284`).
14. نگهبان wedge که خوانندهٔ گیر را قطع می‌کند تا بافر نشست (و در نتیجه L3) آزاد شود (`wedge.go:14-43`).
15. `watchConn`: بستن فوری نشست با اولین خطای خواندن/نوشتن (`stream.go:74-123`).
16. آزمایش انتها‌به‌انتها با `fakeTUN` در هر دو جهت و هم‌زیستی با پورت کاربر.

**در dgtun وجود دارد ولی در l3mtcp سیم‌کشی نشده (موجود در کد، قابل ارجاع):**
17. TCP offload روی TUN: `IFF_VNET_HDR` + `TUNSETOFFLOAD(TUN_F_CSUM|TUN_F_TSO4)`، شکستن بستهٔ GSO تا ۶۴ KB (`splitTCP`)، تکمیل checksum جزئی، و ادغام GRO-مانند در `WriteBatch` (`tun/tun_linux.go:73-96, 155-298`، `tun/offload.go`).
18. `tunBatch` برای نوشتن دسته‌ای در TUN (`engine/tunbatch.go`).
19. بازچین TCP سمت دریافت با نگه‌داشت ۱۵ ms (`engine/reorder.go`).
20. جدول چسبندگی جریان (`sticky`) تا تغییر مجموعهٔ حامل‌ها جریان زنده را جابه‌جا نکند (`engine/dgpool.go:549-556`).
21. صف منصفانهٔ هر حامل با خط سریع برای جریان‌های تعاملی (`engine/dgfq.go`؛ README «Ping under load»).
22. شمارنده‌های جداگانهٔ دورریز به تفکیک علت و فیلدهای `tun_*` در وضعیت (`dgpool.go:930-967`، `status.go:122-138`).

---

## ۱۲. ایده‌هایی که طبق کد/مستندات امتحان و رد شده‌اند

1. **حمل ترافیک کاربر به‌صورت بستهٔ IP روی TLS (TCP-درون-TCP) در `tls`/`l3mtcp`** — در v2 بود؛ «built multi-second queues under load»؛ با هستهٔ جریانی جایگزین شد و hs0 به کانال جانبی تقلیل یافت (`/home/user/hs2-/README.md:659-665`، `hs2-src/README.md:23-26`). نتیجهٔ آزمایشگاه برای l3mtcp محدودشده: گذردهی 34.8 ⇒ 47.1 Mbit/s، تأخیر زیر بار 256 ⇒ 203 ms، اتصال تازه 526 ⇒ 343 ms (`README.md:682-689`).
2. **صف محدود فقط به تعداد بسته** — روی لینک کند ۲۵۶ بسته یعنی چند ثانیه تأخیر («the multi-second pings under load»)؛ سقف زمانی ۶۰ ms افزوده شد (`l3_link.go:57-62`).
3. **نوشتن مستقیم خوانندهٔ TUN روی سوکت** — یک لینک کند همه را می‌ایستاند؛ صف و نویسندهٔ جدا جایگزین شد (`l3_link.go:34-39`).
4. **یک رکورد/فراخوان سیستمی برای هر بسته** — جمع‌کردن در یک نوشتن جایگزین شد (`l3_link.go:40-42`).
5. **keepalive ثابت ۲ ثانیه روی صدها لینک** — «at hundreds of mostly idle links that is most of the idle traffic»؛ حالت کم‌صدا با مذاکره (`l3_link.go:70-77`، `CHANGELOG.md:770-771`).
6. **تکیه بر مهلت ۳۰ ثانیه/keepalive smux برای لینک سیاه‌چاله** — ۲۴–۳۰ ثانیه طول می‌کشید؛ با ۱۲ ثانیه سکوت نشست جایگزین شد (`l3_link.go:82-87`، `README.md:234-235`).
7. **(قاعدهٔ طراحی)** نگاشت جریان به لینک که با مرگ یک لینک همه را جابه‌جا کند — rendezvous انتخاب شد تا فقط جریان‌های لینک مرده بروند (`l3_link.go:45-47`). (در dgtun بعداً معلوم شد rendezvous تنها با هر تغییر مجموعه ~1/n جریان‌ها را جابه‌جا و بازچینی ایجاد می‌کند ⇒ `sticky` افزوده شد، `dgpool.go:549-554`؛ این اصلاح به l3mtcp منتقل نشده است.)
8. **`TCP_NOTSENT_LOWAT` بزرگ یا خاموش** — «In the lab 16-32 KiB were best; 64 KiB and up added latency on slow links and turning it off cost 3-7x» (`tlscarrier/tune_linux.go:12-19`).
9. **keepalive smux ثابت ~۵ s** — اثرانگشت زمانی؛ تصادفی ۴–۸ s شد (`mtcp_link.go:270-278`).
10. **نگه‌داشت بازچین ۲۵ و ۴۰ ms (dgtun)** — روی اتلاف انفجاری بیش از سودش هزینه داشت (۴۰ ms: ~۱۰٪ گذردهی کمتر)؛ ۱۵ ms انتخاب شد (`engine/reorder.go:39-48`).
11. **شنونده‌های MPTCP پیش‌فرض Go** — سوکت MPTCP پذیرفته‌شده `tcp_notsent_lowat` را نادیده می‌گیرد؛ با `godebug multipathtcp=0` خاموش شد (`go.mod:5-7`، `CHANGELOG.md:827-832`).
12. **پینگ کنترل با زمان‌بندی jitter‌دار** — هشدار کاذب اتلاف دانلود را برگرداند؛ ثابت ۳ ثانیه ماند (`CHANGELOG.md:769-770`). (مربوط به کانال کنترل، نه L3.)
13. **بازسازی TUN با هر نسل موتور (رفتار BackPack)** — مسیرهای روی آن را پاک می‌کرد؛ عمداً تکرار نشد (`tun/tun_linux.go:1-5`).

---

## ۱۳. محدودیت‌های شناخته‌شده و مشاهده‌ها

**محدودیت‌های اعلام‌شده در خود پروژه:**
- کانال جانبی «برای ping و ترافیک سبک» است و زیر بار دور می‌ریزد (`l3_link.go:389`، `install.sh:1442-1443`، `README.md:661-666`).
- `tls` یک لینک است و زیر محدودیت به‌ازای هر اتصال از آن سقف بالاتر نمی‌رود (`README.md:690-691`) ⇒ کل hs0 هم روی همان یک لینک است.

**مشاهده‌ها (بدون پیشنهاد تغییر):**
1. **مشاهده — جریان L3 روی لینک زنده دوباره باز نمی‌شود.** `openL3` فقط در `OnLink` است (`stream_iran.go:126-128`). مرگ L3 به‌خاطر `WriteRaw` پنج‌ثانیه‌ای، سکوت ۱۲ ثانیه‌ای که بعداً برطرف شود، یا مهلت ۳۰ ثانیه‌ای، لینک mtcp را برای کاربران زنده می‌گذارد ولی بدون hs0 تا پایان عمرش. در مقابل، «مشکوک» بودن برای کاربران برگشت‌پذیر است (`linkmanager.go:310-317`). در استخر پایدار (مثلاً `min_links=2` بدون جابه‌جایی) ظرفیت hs0 می‌تواند کم شود و تا صفر برسد، بی‌آنکه لاگی بیاید.
2. **مشاهده — `pick` از سلامت و بار بی‌خبر است.** لینک‌های `suspect`/`degraded`/`draining`/`retiring`/spare همچنان جریان L3 می‌گیرند؛ جریانی که روی لینک محدودشده/کند افتاد همان‌جا می‌ماند تا لینک بمیرد.
3. **مشاهده — افزوده شدن لینک هم جریان‌ها را جابه‌جا می‌کند.** با rendezvous، لینک تازه برای ~1/(N+1) جریان‌ها برنده می‌شود؛ بسته‌های در راه روی لینک قبلی و بسته‌های تازه روی لینک جدید ممکن است بی‌ترتیب برسند. در l3mtcp بازچین و جدول `sticky` نیست. رشد/کوچک شدن خودکار استخر (autopilot) این را مکرر می‌کند. (توضیح `l3_link.go:306-307` فقط حالت مرگ را می‌گوید.)
4. **مشاهده — بستن لینک بازنشسته به ترافیک L3 نگاه نمی‌کند.** `drainTick` با `users==0 && Active()==0` می‌بندد (`linkmanager.go:983`) و جریان L3 «بار کاربر» شمرده نمی‌شود (`linkmanager.go:212-216`)؛ پس جریان‌های hs0 هنگام بسته شدن جابه‌جا و بسته‌های صف آن لینک گم می‌شوند.
5. **مشاهده — رفت و برگشت روی لینک‌های مختلف.** هش جهت‌دار و `id`های مستقل دو سمت ⇒ معمولاً دو لینک برای یک جریان. پینگ از راه hs0 ترکیب دو لینک را می‌سنجد، نه یکی.
6. **مشاهده — دانه‌بندی هش.** ICMP و هر پروتکلی جز TCP/UDP فقط با (src,dst,proto) هش می‌شود ⇒ همهٔ پینگ‌های 10.77.0.1↔10.77.0.2 در هر جهت روی **یک** لینک. IPv6 فقط با نشانی‌ها (`pkt[8:40]`) ⇒ همهٔ ترافیک IPv6 میان دو میزبان روی یک لینک. در IPv4 قطعه‌های غیراول بایت‌های بار را به‌جای پورت هش می‌کنند (`l3_link.go:406-408` بدون بررسی fragment offset) ⇒ قطعه‌های یک دیتاگرام ممکن است روی لینک‌های مختلف بروند.
7. **مشاهده — هزینهٔ `pick` به‌ازای هر بسته O(تعداد لینک‌ها)** زیر `RLock` (تا ۳۰۰ بار `mix32` و مقایسه در سقف ۳۰۰ لینک).
8. **مشاهده — سقف ۶۰ ms فقط زمان صف L3 را می‌سنجد.** زمان درون heap زمان‌بند smux، در انتظار پنجره، و در بافر ارسال هسته شمرده نمی‌شود. وقتی `WriteRaw` تا ۵ s گیر کند، صف تا ۲۵۶ پر و سپس یک‌جا به‌عنوان «کهنه» دور ریخته می‌شود.
9. **مشاهده — بی‌اولویتی در smux.** قاب‌های L3 با جریان‌های کاربرِ همان لینک به ترتیب FIFO (کلاس DATA، شمارهٔ درخواست) نوبت می‌گیرند؛ با N جریان حجیم فعال، یک دستهٔ L3 پشت حداکثر N قاب ۱۶ KiB می‌ماند.
10. **مشاهده — سربار بستهٔ کوچک.** شکل‌دهنده هر نوشتن کوچک را تا اندازهٔ نمونه (میانگین ≈ ۹۹۱ بایت) پد می‌کند؛ یک پینگ یا keepalive تنها حدود یک کیلوبایت روی سیم می‌شود (محاسبه از `obfs/shaper.go:41-42` و `shape.go:60-100`).
11. **مشاهده — بدون offload/دسته‌نویسی روی hs0.** یک goroutine با یک `read()` برای هر بسته و برای هر لینک یک `write()` برای هر بسته؛ اگر hs0 بار حجیم ببرد، این مسیر از نظر پردازنده همان چیزی است که در dgtun با فاز W2 کاهش یافت (۴۵۰ ⇒ ۶۵۰ Mbit/s و ۲۷–۳۹٪ پردازندهٔ کمتر در آزمایشگاه dgtun، `CHANGELOG.md:1252-1268`). **نامطمئن:** اندازهٔ این گلوگاه برای l3mtcp اندازه‌گیری نشده است.
12. **مشاهده — `watchSession` در wedge.** `rdCalls` وقتی `recvLoop` smux روی بافر پر نشست پارک شده هم تکان نمی‌خورد؛ اگر wedge ≥ ۱۲ s بماند L3 رها می‌شود، هرچند توضیح کد مهلت ۳۰ ثانیه را «for a link whose session still talks (a wedge, ...)» می‌داند (`l3_link.go:82-84`). نگهبان wedge معمولاً پس از ≥۴ s کار می‌کند، پس این حالت نادر است (**نامطمئن**).
13. **مشاهده — رؤیت‌پذیری کم.** وضعیت l3mtcp هیچ شمارندهٔ TUN، تعداد لینک L3 یا تفکیک علت دورریز ندارد؛ مرگ L3 لاگ نمی‌شود.
14. **مشاهده — کد مرده/آزمونی.** `l3Set.removeDead`، `count`، `closeAll`، `l3DeadAfter` و مسیر `newL3Link` روی حامل TLS خام در تولید استفاده نمی‌شوند؛ `remove` دم برش را nil نمی‌کند (`l3_link.go:260-270`) برخلاف `removeDead`.
15. **مشاهده — حلقهٔ خطای خواندن TUN بدون مکث** (`l3_link.go:344-350`): خطای ماندگار پیش از پایان ctx می‌تواند حلقهٔ داغ بسازد.
16. **مشاهده — `dev.Write` بی‌بررسی خطا و هم‌گام** در goroutine خوانندهٔ هر لینک (`l3_link.go:374`).
17. **مشاهده — MTU/MSS.** hs2 هیچ MSS clamp نمی‌گذارد؛ ترافیک عبوری (اگر اپراتور forward کند) به PMTUD متکی است. چون بسته‌ها روی جریان بایتی می‌روند، MTU رابط hs0 به MTU مسیر بیرونی وابسته نیست؛ محدودیت‌های واقعی کد: بافر خواندن `MTU+128` و سقف قاب `65536` (`stream.go:227`).
18. **مشاهده — TCP درون TCP برای هر ترافیک حجیمی که روی hs0 بیاید** (حالت «تونل مسیریابی‌شدهٔ خالص»): گذردهی هر جریان به یک لینک و به دورریزهای ۶۰ ms/۲۵۶ بسته محدود است؛ همان چیزی که v3 برای پورت‌های کاربر کنار گذاشت.
19. **مشاهده — حافظهٔ بدترین حالت** حدود `256 × (MTU+128)` بایت برای هر لینک (≈ ۱۱۱ MB در ۳۰۰ لینک با MTU 1320؛ محاسبهٔ من).
20. **مشاهده — `ip link del <iface>` در شروع** هر رابط هم‌نام را بی‌پرسش حذف می‌کند (`tun_linux.go:100-102`)؛ نصب‌کننده پیش‌تر `iface_taken` را بررسی می‌کند (`install.sh:1656`).
21. **مشاهده — `routes.go` با مسیریابی IP روی hs0 ارتباطی ندارد**؛ فقط هدف جریان‌های TCP/UDP کاربر را روی خروجی تعیین می‌کند (`routes.go:13-29`). مسیرهای IP روی hs0 فقط ‎/30 متصل و ‎/32 همتا هستند.

---

## ۱۴. ارجاع به زیرسیستم‌های دیگر

| چه کسی | چه چیزی را صدا می‌زند | محل |
|---|---|---|
| `cmd/hs2/main.go` `runStream` | `tun.Open`، `engine.RunIran`/`RunKharej` با `TUN` | `main.go:453-551` |
| `cmd/hs2/main.go` `runDgTun` | `tun.OpenWith(... Offload)`؛ آمار offload در وضعیت | `main.go:829-881` |
| `engine.RunIran` | `l3Set{}`، `pumpTun`، `logDrops`، و در `OnLink`: `openL3` | `stream_iran.go:90-129` |
| `LinkManager` (زیرسیستم استخر) | `OnLink` برای هر لینک تازه (شماره‌گیری یا وارونه) | `linkmanager.go:434-435, 946-947` |
| `engine.RunKharej` / `runKharejReverse` / `serveReverseLink` | `l3Set{}`، `serveStream(..., l3, ...)` | `stream_kharej.go:87-157`، `stream_reverse.go:109-192` |
| `serveStream` | `newStreamL3Link`، `l3.serveLink` | `stream_kharej.go:245-253` |
| `newStreamL3Link` | `newStreamPkt`، `peerL3Quiet` (`peerinfo.go`)، `sessReadsOf` ⇐ `linkMeter.guard.w` (`stream.go`/`wedge.go`)، `jitterAround` (`exit_pool.go:565`) | `l3_link.go:125-138` |
| `writeLoop` | `tlscarrier.AppendFrame`، `core.TypeData/TypePing`، `core.KeepalivePad` | `l3_link.go:210, 235` |
| `streamPkt` | `smux.Stream.Write/Read/SetDeadline/Close` | `stream.go:206-241` |
| `newSession` | `newShapedConn` (`shape.go`)، `meteredConn` (`health.go`)، `watchConn`، `smux.Server/Client(newSmuxConfig())`، `sessGuard` (`wedge.go`) | `stream.go:172-203` |
| `mtcpLink.OpenRawStream` | `smux.Session.OpenStream` (بدون شمارش کاربر) | `mtcp_link.go:183` |
| حامل TLS | `tuneTCP` (BBR/NOTSENT/USER_TIMEOUT)، uTLS Chrome 133، احراز با EKM | `tlscarrier/carrier.go:161-217`، `tlscarrier/tune_linux.go` |
| `dgPool` (dgtun) | `flowHash` (همان تابع L3)، `newTunBatch`، `newReorderer`، `pumpTun` خودش | `dgpool.go:670-671, 927-967` |
| موتور قدیمی (`reality`/`noise`/`udp`/`auto`) | `tun.Open` جداگانه، مسیر تک‌حامل | `engine/engine.go:118-126` |
| `cmd/hs2/doctor.go` `checkTun` | وجود/بالا بودن رابط و نشانی آن | `doctor.go:214-248` |
| `cmd/hs2/check.go` | اعتبار `iface`/`local_cidr`/`peer_ip`/`mtu` | `check.go:333-346` |
| `install/install.sh` | انتخاب `l3mtcp`/`tls` زیر «tun → tcp»، زیرشبکهٔ ‎/30، MTU، نام رابط، اثبات با `ping -I <iface> <peer>` | `install.sh:1405-1450, 1647-1672, 930-956` |
| `tune` | sysctlهای سراسری (`rp_filter=2`، `tcp_mtu_probing=1`، `default_qdisc`) | `tune/tune.go:336-371` |

---

### پیوست: فهرست فایل‌هایی که کامل خوانده شد
`engine/l3_link.go`، `engine/tunbatch.go`، `engine/routes.go`، `engine/reorder.go`، `tun/tun_linux.go`، `tun/offload.go`، `engine/l3_link_test.go`، `engine/tun_mode_test.go`، `engine/tunbatch_test.go`، `tun/tun_linux_test.go`، `engine/stream.go`، `engine/stream_iran.go`، `engine/stream_kharej.go`، `engine/mtcp_link.go`، `tlscarrier/carrier.go`، `tlscarrier/tune_linux.go`؛ و بخش‌های مرتبط از `engine/stream_reverse.go`، `engine/peerinfo.go`، `engine/wedge.go`، `engine/shape.go`، `engine/linkmanager.go`، `engine/dgpool.go`، `engine/health.go`، `engine/reorder_test.go`، `engine/routes_test.go` (فهرست آزمون‌ها)، `tun/offload_test.go` (فهرست آزمون‌ها)، `cmd/hs2/main.go`، `cmd/hs2/check.go`، `cmd/hs2/doctor.go`، `cmd/hs2/status.go`، `obfs/shaper.go`، `core/shape.go`، `core/frame.go`، `tune/tune.go`، `install/install.sh`، `lab/run.sh`، `install/tests/tun_ports_test.py`، `smux@v1.5.24/{stream.go,session.go,shaper.go,frame.go}`، `README.md`، `CHANGELOG.md`، `hs2-src/README.md`، `hs2-src/BUILD.md`.

آزمون‌های اجراشده (بدون تغییر در مخزن): همهٔ آزمون‌های L3، tun-mode، reorder و tunbatch در `./engine` گذشتند؛ در `./tun` همه جز `TestOffloadDeviceUDP` گذشتند که به دلیل نبودن فرمان `ip` در این محیط شکست خورد.
