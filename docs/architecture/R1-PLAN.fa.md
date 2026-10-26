# برنامهٔ اجرایی R1: زمان‌بند، رده‌بند، نگهبان صف، C4-lite، قرارداد سیم ۳، بازیابی لینک

> **برای ایجنت‌های اجراکننده:** زیرمهارت لازم: `superpowers:subagent-driven-development` (هر کار با یک زیرعامل تازه و یک بازبین تازه). گام‌ها با `- [ ]` علامت می‌خورند.

**هدف:** فاز R1 سند بازطراحی، با روش ثابت هر فاز (§۵.۱۰)، از خط پایه تا تصمیم پایان فاز.

**معماری:**
- هسته‌های خالص و قطعی که سند کامل تعریفشان کرده، یک بار و مشترک با آزمون مدل ساخته می‌شوند: قرارداد سیم ۳، C1، الگوریتم C2، C3، C4-lite، PROXY protocol، C15 و `hs2-safe-apply`.
- سپس دو نمونه از دو زاویهٔ سند (§۵.۱) آن‌ها را به مسیر نوشتن وصل می‌کنند:
  - **زاویهٔ الف:** زمان‌بند درون smux.
  - **زاویهٔ ب:** زمان‌بند در موتور و smux فقط فریم‌ساز.
- داور روی چیدمان بزرگ یکی را برمی‌گزیند، یا ترکیب هر دو را.

**ابزار:** Go 1.27.3 (`/usr/local/go` روی خارج)، `CGO_ENABLED=0`، لینوکس، `tc`/netns، bash، python3.

**سند مرجع:** `docs/architecture/REDESIGN.fa.md` (شاخهٔ `redesign-doc`)، به‌ویژه این بخش‌ها:
- §۳.۱ تا §۳.۴ و §۳.۱۶
- §۴.۱ تا §۴.۳
- §۵.۱ و §۵.۱۰
- §۶

هر عددی که این برنامه از سند می‌آورد، در سند هم هست. اگر برنامه و سند ناهم‌خوان بودند، سند درست است.

## قیدهای سراسری

- **پایه:** build `1c23629533c9` (ثبت `b780b63`، شاخهٔ `claude/smux-buffer-constants-fv9bu8`).
- **شاخهٔ کار:** `redesign-r1` در worktree `/root/hs2-wt/r1` روی خارج.
- **امنیت:** کلید، رمز مشترک، گواهی و نشانی IP سرورها هرگز در مخزن، پیام‌های ثبت و گزارش‌ها نمی‌آید.
- **کار سنگین:**
  - ساخت و `go test` فقط روی خارج، و هرگز در حین اجرای میدانی.
  - هیچ کار سنگینی روی ایران.
- **درگاه‌های آزمایشی:**
  - ایران 5311/5312 به خارج 5211 (iperf3) و 5212 (echo).
  - **هرگز 5201.**
- **پیام ثبت:**
  - فارسی، با خط انتساب `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
  - ثبتی که گام n روش را می‌بندد، عنوانش با «R1 گام n:» آغاز می‌شود. جدول پیشرفت از روی همین عنوان تیک می‌زند.
- **سیم ۳ (§۴.۱):**
  - `ver = 3`؛ برای R1: `min_level = max_level = 1`؛ `caps` از نوع u32 و big-endian.
  - برچسب‌های HMAC: `"hs2-auth-v3"` و `"hs2-srv-v3"`.
  - رکورد مشتری: `[nonce 16][tag 16][hdr 7][padlen 2][pad 273..713]`.
  - اثبات کارگزار: `[tag 16][hdr 6][padlen 2][pad 114..474]`.
  - همهٔ سرآیندها زیر HMAC‌اند. طول کل رکوردها روی سیم دقیقاً همان امروز می‌ماند: مشتری 314..754 و کارگزار 138..498.
- **بار SYN (§۴.۲):** ۱۳ بایت:
  - `user tag u32 BE`
  - `class/role u8`: بیت‌های ۰–۱ اشارهٔ کلاس، بیت‌های ۲–۳ نقش (۰ در R1)، بیت‌های ۴–۷ صفر.
  - `group id u32` (۰ در R1)
  - `seq base u32` (۰ در R1)

  جریانی با SYN بی‌بار یا با طول دیگر رد می‌شود و شمارندهٔ `bad syn` بالا می‌رود. سرآیند جریان کاربر فقط سه‌بایتی `[kind][port u16]` است.
- **توصیف‌گر TUN (§۴.۳):** `[type 1][len 3][pad 3][user tag 4][flow hash 4]` = ۱۵ بایت.
- **C1 (§۳.۱):**
  - ارزیابی هر ۱۰۰ms.
  - light یعنی کمتر از ۶۴ KiB در ۲s **و** کمتر از ۱۶ KiB در ۲۰۰ms.
  - جریان unknown تا ۳۲ KiB نخست مثل light رفتار می‌شود.
  - «light تأییدشده» یعنی دو ارزیابی پیاپی light.
  - bulk پس از ۲s سکوت light می‌شود.
  - برچسب IPv6 روی پیشوند /64 ساخته می‌شود (قابل تنظیم).
  - پیمایش سرآیندهای الحاقی IPv6 حداکثر ۸ سرآیند یا ۲۵۶ بایت است؛ بیش از آن کلید سه‌تایی می‌شود.
  - ICMP و ICMPv6 echo همیشه light‌اند.
- **C2 (§۳.۲):**
  - **رده‌ها:** کنترل، سپس خط تأخیر، سپس حجیم دوسطحی.
  - **خط تأخیر:** اعتبار هر کاربر سطل توکن ۱۶۰ KiB/s با انفجار ۳۲ KiB؛ فریم خط حداکثر ۲ KiB.
  - **کف هر کاربر:** `F_u = min(D_u, C/N_active)`، هر ۱۰۰ms، که `D_u` آمادگی ارسال ۲۰۰ms اخیر کاربر است. مازاد به ازای جریان پخش می‌شود.
  - **فریم حجیم:** `clamp(rate × 5ms, 1 KiB, 16 KiB)`. پیش از C4، `rate` میانگین ۱s `wrBytes` از `meteredConn` است.
  - **صف‌های حجیم:** ۶۴ فریم معلق مسیر جریان به ازای کاربر. TUN: ۶۴ بسته و ۲۰ms به ازای جریان، ۱۰۲۴ بسته به ازای کاربر. در سرریز، چاق‌ترین کاربر اول.
  - **حافظه:** `user_mem_share` ۶۴ MiB، کف به‌اضافهٔ قرض.
- **C3 (§۳.۳):**
  - هر ۵۰۰ms: `TCP_NOTSENT_LOWAT = clamp(rate × 2ms, max(2×MSS, 4 KiB), 256 KiB)`. فقط وقتی دوباره نوشته می‌شود که مقدار دست‌کم ۲۵٪ عوض شده باشد.
  - **دروازهٔ نوشتن:** روشن زیر ۲۰ و خاموش بالای ۳۰ Mbit/s.
  - `cap = max(rate × 2ms, 2×MSS)`.
  - کم‌آب سوکت در بازهٔ دروازه: `max(cap/2, MSS)`.
- **C4-lite (§۳.۴):**
  - `SO_MAX_PACING_RATE = 1.25 × max(delivery_rate در ۲s)` با کف 1 MiB/s، روی همهٔ سوکت‌های لینک در هر دو سر.
  - از نخستین نمونهٔ غیر app-limited اعمال و در بیکاری برداشته می‌شود.
  - تور pacing سقف خودش را به C4-lite می‌دهد و کمینهٔ دو سقف اعمال می‌شود.
- **کلیدهای خاموشی:** `HS2_TUNE_CLASS=0`، `HS2_TUNE_SCHED=0`، `HS2_TUNE_HQG=0`، `HS2_TUNE_SENDCAP=0`. هر کلید بیت خودش را هم از `caps` برمی‌دارد.
- **سطرهای وضعیت:** همان متن‌های سند (§۳.۱ تا §۳.۴ و §۳.۱۶ و §۴.۱)، بی تغییر.
- **C15 (§۳.۱۶):**
  - فرمان‌ها: `hs2 link -c <config>`، `--list`، `--decode`، و پرچم‌های `--endpoint`، `--domain`، `--transport`، `--udp`.
  - کدهای خروج: ۰ لینک کامل، ۳ فیلد کم، ۲ پیکربندی نامعتبر، ۴ بی دسترسی.
  - بلوک `link` در پیکربندی: `{"endpoint","domain","transport","udp","panel"}`.
  - لینک بی فیلد افزوده بایت‌به‌بایت همان لینک امروز است.
  - هیچ اقدام شبکه‌ای و هیچ ثبت لینک.
- **PROXY protocol:** نسخهٔ ۱ و ۲، فقط روی درگاه‌هایی که پرچم `proxy` دارند. نشانی‌های مجاز `proxy_from` با پیش‌فرض loopback است. اتصال بی سرآیند معتبر بسته می‌شود.
- **بازگشت‌پذیری:**
  - باینری `READY=1` را روی `$NOTIFY_SOCKET` می‌فرستد.
  - `scripts/hs2-safe-apply` به مخزن می‌آید: واحد از `HS2_SAFE_UNIT`، پیکربندی از `ExecStart` همان واحد، و کل پوشهٔ drop-inها پشتیبان‌گیری و بازگردانده می‌شود.
  - بازگشت خودکار اگر hs2 تا ۶۰s پایدار نماند، یا همتا تا ۳۰۰s به پینگ پاسخ ندهد.
  - از R1 هر تغییر سرویس فقط از راه drop-in است، نه فایل اصلی سرویس.
- **پذیرش R1 (§۵.۱):**
  - **کاوشگر:** p99 ≤ `RTT_floor + max(20ms, 3 × MTU/rate)`.
  - **P10 (الف تا هـ):** کاربر تنها ≥ ۰٫۹۵×؛ کاربر ۱ اتصالی کنار ۱۶ اتصالی ≥ ۰٫۹× کفش؛ جمع ≥ ۰٫۹۷×.
  - **پینگ TUN:** p99 ≤ `RTT_floor + max(10ms, 3 × MTU/rate)`.
  - **توان:** ≥ ۰٫۹۷× پایه.
  - **«بازگشت»:** سبز.
  - **C15:** رفت و برگشت بایت‌به‌بایت.
  - **پردازنده:** فقط گزارش می‌شود و دروازه نیست.
- **مقیاس (P11):**
  - همهٔ معیارها روی N ∈ {۱، ۴، ۱۶، ۶۴، ۱۲۸، ۳۰۰} و سقف خود سرور، در هر دو جهت.
  - هیچ ثابتی به N خاصی وابسته نیست.
  - کار هر لینک O(1) در هر تیک است.

## تمرکز بازبینی

پنج حالتی که سند می‌طلبد ولی آزمون‌های کارها به‌خودی‌خود نمی‌پوشانند:

1. **نصب نخستین نسخهٔ سیم ۳ روی تونل زنده.** باینری R1 با باینری پایه (سیم ۲) حرف نمی‌زند. اگر خارج نصب شود و ایران دیر برسد، قاعدهٔ پینگ ۳۰۰s ممکن است خارج را برگرداند.
   - انتظار: نصب R1 هر دو سر را در یک پنجره می‌گذارد، یا پنجرهٔ همتا قابل تنظیم است.
   - آزمون: در کار ۱۴ (`HS2_SAFE_PEER_WAIT`).
2. **کاربران بسیار پشت یک IP (CGNAT) و نشانی‌های چرخان IPv6.** هر IPv4 یک کاربر است و هر /64 یک کاربر.
   - آزمون: در کار ۸ (`TestUserTagIPv6Slash64`).
3. **کاربری با هزاران جریان یا نویسندهٔ مسدود پشت سوکت کاربر.** کف دیگران و فریم کنترلی نباید آسیب ببینند.
   - آزمون: در کار ۹ (`TestSchedThousandStreamsUser`).
4. **هستهٔ لینوکسی که این‌ها را ندارد:** `TCP_NOTSENT_LOWAT`، `SIOCOUTQNSD` یا `tcpi_delivery_rate`.
   - انتظار: رفتار امروز، با یک خط لاگ.
   - آزمون: در کار ۱۰ و ۱۱ (فراخوانی سیستمی ساختگی که خطا می‌دهد).
5. **تونل‌های نصب‌شده پیش از R1، یا پیکربندی دستی‌ویرایش‌شده، در `hs2 link`.**
   - انتظار: امتناع با نام فیلد، نه لینک خراب.
   - آزمون: در کار ۱۳.

---

## گام ۱ روش: شناخت مسئله و خط پایه

### کار ۱: شاخهٔ R1

**فایل‌ها:** شاخهٔ تازهٔ `redesign-r1` و worktree `/root/hs2-wt/r1`.

- [ ] **گام ۱:** شاخه را بساز:
  ```bash
  cd /root/hs2-repo
  git worktree add -b redesign-r1 /root/hs2-wt/r1 b780b63
  ```
- [ ] **گام ۲:** سند را ادغام کن:
  ```bash
  cd /root/hs2-wt/r1
  git merge --no-ff redesign-doc
  ```
  ناهم‌خوانی احتمالی در `docs/architecture/ROADMAP.fa.md` را با نگه داشتن **هر دو** بخش حل کن: «بستن Z5 گام ۲» و «بازطراحی (R1–R9)».
- [ ] **گام ۳:** پایه را بساز و بسنج:
  ```bash
  cd hs2-src && go build ./... && go test ./... -count=1
  ```
  انتظار: سبز، همان پایهٔ `b780b63`. هر آزمون لرزان را با نام و دفعات در `docs/architecture/wip/r1/flaky.fa.md` بنویس. این آزمون‌ها در کار ۱۸ رفع یا با دلیل قرنطینه می‌شوند.

### کار ۲: چیدمان بزرگ (`rig-scale`)

**فایل‌ها (تازه):**
- `docs/architecture/wip/rig-scale/README.fa.md`
- `rig.sh`
- `run.sh`
- `ana.py`
- `rigload/` (یک ماژول Go جدا با `go.mod` خودش، مثل `rig-core/plaintest`)
- `rig_test.sh`

**رابط‌ها:**
- **فرآورده:** `run.sh PLAN OUT` که هر سطر PLAN یک خانه است، مثل:
  `N=16 RATE=50 RTT=72 LOSS=0 MODE=fqcodel|pfifo:LIMIT|police:RATE|agg:RATE USERS=200 MIX=s5|lone16|one_vs_16|newheavy|tunping BIN=old|new`

  خروجی: یک سطر JSON در `OUT/res.jsonl` به ازای هر خانه.
- **فرآورده:** `ana.py OUT/res.jsonl` جدول میانه و کمینه/بیشینهٔ ۳ تکرار، و حکم P10 (الف تا هـ) و معیار p99 هر خانه را می‌دهد.

- [ ] **گام ۱:** `rig.sh`:
  - سه فضای نام `r-edge`، `r-mid` و `r-exit`.
  - گلوگاه در `r-mid` با `htb rate RATE`، و یکی از این‌ها:
    - `fq_codel`
    - `pfifo limit LIMIT`
    - `police` به ازای اتصال (`flower` روی ۵تایی)
    - شکل‌دهندهٔ جمعی
  - با `netem delay RTT/2 loss LOSS` در هر جهت.
  - برای کاربران چند نشانی منبع با `ip addr add`: تا ۵۰۰ نشانی IPv4 و یک /64 برای IPv6.
  - هر خانه پیش از اجرا CR را از PLAN پاک می‌کند (`tr -d '\r'`).
- [ ] **گام ۲:** `rigload`، یک برنامهٔ Go با این نقش‌ها:
  - `bulkdown`، `bulkup`
  - `probe`: ۳۲ بایت هر ۲۰۰ms و ثبت RTT
  - `tunping`: ICMP روی hs0 فضای نام

  هر نقش از نشانی منبع خودش اجرا می‌شود و خروجی JSON می‌دهد: توان هر کاربر، p50/p99/max کاوشگر و پینگ. کاربر به‌صورت «نشانی منبع» تعریف می‌شود.
- [ ] **گام ۳:** `run.sh`:
  - دو پردازهٔ hs2 (`BIN=old` پایه، `BIN=new` شاخه) با `max_links=N` و تونل معکوس l3mtcp، یکی در `r-edge` و یکی در `r-exit`.
  - ترتیب ABBABA با ۳ تکرار.
  - CPU هر دو پردازه با `pidstat`، بازارسال با `ss -ti`، و سطرهای `hs2 status`.
- [ ] **گام ۴:** `ana.py`: حکم‌های P10 با تعریف‌های §۵.۱ و `RTT_floor` (صدک ۱ کاوشگر همان اجرا).
- [ ] **گام ۵:** `rig_test.sh`، آزمون خود چیدمان:
  - خانهٔ `N=1 RATE=50 RTT=72 MIX=lone16 BIN=old` را دو بار اجرا کن.
  - انتظار: `res.jsonl` دو سطر معتبر دارد، توان ≥ ۰٫۸۵× RATE، و اختلاف دو تکرار < ۱۰٪.
  - خانهٔ `N=300` را هم یک بار اجرا کن. اگر میزبان تاب نیاورد، بزرگ‌ترین N را که تاب می‌آورد در README بنویس.
- [ ] **گام ۶:** ثبت: `rig-scale: چیدمان بزرگ با N لینک، کاربران مصنوعی و حکم P10`.

### کار ۳: ثبت کوتاه ویژگی‌های جریان روی لبه، و نشست برچسب‌دار

**فایل‌ها (تازه):**
- `docs/architecture/wip/r1/flowcap/flowcap.go` (برنامه‌ای جدا که روی ایران اجرا می‌شود)
- `docs/architecture/wip/r1/flowcap/README.fa.md`

**قید:** بی نشانی و بی محتوا. فقط برای هر جریان یک شناسهٔ تصادفی، و بایت هر جهت در بازه‌های ۱۰۰ms. سبک، و هرگز روی مسیر داغ hs2.

- [ ] **گام ۱:** `flowcap` را بنویس.
  - شمارنده‌ها را از `ss -tin` یا conntrack روی درگاه‌های کاربر می‌خواند، هر ۱۰۰ms.
  - خروجی در `/root/r1-flowcap/` به‌صورت `jsonl.gz` است. سطرها `{"f":<id>,"t":<ms>,"up":<B>,"dn":<B>}` هستند، **بی هیچ نشانی و درگاه**.
- [ ] **گام ۲:** آزمون واحد: هیچ نشانی‌ای در خروجی نیست.
  ```go
  func TestFlowcapNoAddresses(t *testing.T) // خروجی یک اجرای نمونه با netip.ParseAddr هیچ رشته‌ای را نشانی نمی‌شناسد
  ```
- [x] **گام ۳:** آن را با `systemd-run` و `CPUQuota=5%` (با `CPUQuotaPeriodSec=1s`) روی ایران اجرا کن، **حداکثر یک ساعت** (تصمیم صاحب پروژه؛ REDESIGN بخش ۱۰ بند ۶۳)، سپس به خارج بیاور. اجرا شد: ۲۰ دقیقه، ۲۹۲۳ جریان، ۳٫۲ MB؛ کل پرونده در `hs2-src/engine/testdata/c1_replay.jsonl.gz`.
- [x] **گام ۳ب: نشست برچسب‌دار.** در زمان خلوت خارج (همراه کار ۴)، flowcap فقط روی درگاه‌های آزمایشی ۵۳۱۱/۵۳۱۲ (≤ ۱۵ دقیقه) و الگوهای شناخته یکی پس از دیگری از راه همان درگاه‌ها: دانلود، آپلود، درخواست-پاسخ، انفجار ۲۰ KiB، ۱s روشن/۲s خاموش، شبه‌VoIP. حکم درست هر جریان از بازهٔ زمانی‌اش در `hs2-src/engine/testdata/c1_labeled.jsonl.gz` (سطرها `{"f","t","up","dn","want"}`)؛ آزمون `TestClassifierLabeled` نرخ خطای حکم را با همان معیار §۳.۱ می‌سنجد. اجرا شد: ۱۵ جریان، ۶۲۳۸ سطر، ۲۵ KB. درگاه‌های آزمایشی در پیکربندی تولید باز نیستند، پس الگوها از درون hs0 به همین دو درگاه روی لبه رفتند (`wip/r1/baseline.fa.md`).
  - نمونهٔ فشرده‌شده (≤ ۵ MB) را در `hs2-src/engine/testdata/c1_replay.jsonl.gz` ثبت کن.
  - این کار موازی با کارهای ۵ تا ۱۴ پیش می‌رود.

### کار ۴: خط پایه

- [x] **گام ۱:** روی چیدمان با `BIN=old`، خانه‌های §۵.۱ را اجرا کن (زیرمجموعهٔ اولویت‌دار در سقف یک ساعت: ۲۴ خانهٔ P10، ۱۶ خانهٔ `s5` و ۳ خانهٔ افزوده؛ خانه‌های اجرانشده در `wip/r1/baseline.fa.md`):
  - نرخ‌ها {۱، ۵، ۱۰، ۲۵، ۵۰، ۱۰۰، ۱۰۰۰} × RTT {۲۰، ۷۲، ۱۵۰} × {۰، ۰٫۱٪}
  - بار: ۴ دانلود + ۲ آپلود + کاوشگر، روی `max_links=1` و ۸
  - به‌اضافهٔ این خانه‌ها روی N ∈ {۱، ۴، ۱۶، ۶۴، ۱۲۸، ۳۰۰}: `lone16`، `one_vs_16`، `newheavy`، `tunping`
- [x] **گام ۲:** در میدان، S5 را سه بار روی پایه بگیر (ابزار `/root/z5b`) تا کف میدان معلوم شود. سه بار اجرا شد، از درون hs0، چون درگاه‌های آزمایشی در تولید باز نیستند.
- [x] **گام ۳:** نتیجه را در `docs/architecture/wip/r1/baseline.fa.md` بنویس: جدول، و جاهایی که پایه معیار R1 را می‌شکند.
- [ ] **گام ۴:** ثبت با عنوان `R1 گام 1: خط پایه روی چیدمان بزرگ و میدان` و ارسال (fetch به رایانه، سپس push). ثبت در شاخهٔ `r1-baseline` انجام شد؛ ارسال با سرپرست.

---

## گام ۲ روش: ساخت دو طرح مستقل

کارهای ۵ تا ۱۴ بلوک‌های مشترک قطعی‌اند و هر دو نمونه از آن‌ها استفاده می‌کنند.
- کارهای ۵، ۶، ۷، ۱۲، ۱۳ و ۱۴ به هم وابسته نیستند و می‌توانند موازی پیش بروند.
- کار ۹ به کار ۸ نیاز دارد.
- کارهای ۱۵ و ۱۶ (دو نمونه) به همهٔ کارهای ۵ تا ۱۴ نیاز دارند.

فرمان آزمون همه‌جا: `cd /root/hs2-wt/r1/hs2-src && go test <pkg> -run <name> -count=1`.

### کار ۵: سیم ۳، سرآیند احراز، مذاکرهٔ سطح، رد همتای نسخهٔ ۲

**فایل‌ها:**
- تغییر: `hs2-src/tlscarrier/auth.go`، `hs2-src/tlscarrier/carrier.go` (فیلدهای نتیجه)، و جاهایی که `makeClientAuth`/`parseClientAuth`/`makeServerProof`/`readServerProof` صدا زده می‌شوند.
- آزمون: `hs2-src/tlscarrier/auth_v3_test.go`

**رابط‌ها (فرآورده):**
```go
const protoVersion byte = 3
type AuthHdr struct{ Ver, MinLevel, MaxLevel byte; Caps uint32 }   // 7 B on the wire
type ProofHdr struct{ Ver, Level byte; Caps uint32 }                // 6 B on the wire
const (CapClass uint32 = 1 << iota; CapSched; CapHostQ; CapSendCap)
func LocalAuthHdr() AuthHdr                                          // levels 1..1, caps minus the bits whose kill switch is set
func negotiate(c AuthHdr, srvMin, srvMax byte, srvCaps uint32) (level byte, caps uint32, ok bool)
func makeClientAuth(key, ekm []byte, h AuthHdr) (rec, nonce []byte)
func parseClientAuth(key, ekm, first []byte) (nonce []byte, h AuthHdr, more int, v2 bool, ok bool)
func makeServerProof(key, nonce, ekm []byte, p ProofHdr) []byte
func readServerProof(r io.Reader, key, nonce, ekm []byte) (ProofHdr, error)
var ErrPeerV2 = errors.New("peer runs protocol v2; upgrade both servers")
var ErrLevel  = errors.New("no common protocol level with the peer")
func (c *Carrier) Level() byte
func (c *Carrier) Caps() uint32
```

- [ ] **گام ۱: آزمون‌های شکست‌خورده**
  ```go
  func TestAuthV3RoundTrip(t *testing.T)            // h برگشتی == h فرستاده؛ ProofHdr برگشتی == فرستاده
  func TestAuthV3LengthsUnchanged(t *testing.T)     // 2000 رکورد: مشتری 314<=len<=754، کارگزار 138<=len<=498، و هر دو سر بازه دیده شود
  func TestAuthV3HeaderUnderMAC(t *testing.T)       // تغییر هر بیت hdr ⇒ parseClientAuth ok=false
  func TestAuthV3SeesV2Peer(t *testing.T)           // رکورد ساخته‌شده با منطق v2 ⇒ ok=false, v2=true
  func TestNegotiate(t *testing.T)                  // جدول: (1..1 با 1..1)⇒1؛ (1..2 با 2..3)⇒2؛ (1..1 با 2..2)⇒ok=false؛ caps = AND
  func FuzzParseClientAuth(f *testing.F)            // بی panic، بی خواندن بیرون از first
  ```
- [ ] **گام ۲:** آزمون‌ها را اجرا کن: `go test ./tlscarrier/ -run 'AuthV3|Negotiate' -count=1`. انتظار: شکست، چون نام‌ها تعریف نشده‌اند.
- [ ] **گام ۳:** پیاده‌سازی طبق §۴.۱:
  - برچسب `hs2-auth-v3` روی `nonce‖minute‖EKM‖hdr`، و `hs2-srv-v3` روی `nonce‖EKM‖hdr`.
  - پد مشتری 273..713 و کارگزار 114..474.
  - اگر برچسب v3 نخورد، کارگزار برچسب v2 را **فقط** برای لاگ می‌آزماید، و اگر هیچ‌کدام نخورد، مثل امروز بازپخش به backend.
  - لاگ `peer runs protocol v2; upgrade both servers` حداکثر یکی در دقیقه برای هر همتا.
  - سطر وضعیت `protocol: v3 level L (peer levels A-B, caps 0xC); refused v2 peers: N, refused peers (level): M (last t s ago)`.
- [ ] **گام ۴:** همان فرمان را دوباره اجرا کن، و سپس `go test ./tlscarrier/ -fuzz FuzzParseClientAuth -fuzztime 60s`. انتظار: سبز.
- [ ] **گام ۵:** ثبت: `سیم ۳: سرآیند نسخه، بازهٔ سطح و caps زیر HMAC؛ رد همتای v2`.

### کار ۶: بار SYN سیزده‌بایتی و سرآیند سه‌بایتی جریان

**فایل‌ها:**
- تغییر: `hs2-src/third_party/smux/session.go` (`OpenStream`، `recvLoop` در بخش cmdSYN)، `hs2-src/third_party/smux/stream.go`، و `hs2-src/engine/stream_iran.go` (`userStreamHeader` و هر جای باز کردن جریان).
- تغییر: `hs2-src/engine/stream_kharej.go` (`serveStream`: خواندن برچسب از SYN).
- آزمون: `hs2-src/third_party/smux/syn_test.go`، `hs2-src/engine/streamhdr_v3_test.go`

**رابط‌ها (فرآورده):**
```go
// smux
const SynPayloadLen = 13
func EncodeSyn(userTag uint32, classHint byte) [SynPayloadLen]byte   // group id = 0, seq base = 0, role = 0
func (s *Session) OpenStreamSyn(p [SynPayloadLen]byte) (*Stream, error)
func (st *Stream) Syn() (userTag uint32, classHint byte)
func (s *Session) BadSyn() uint64                                    // cumulative counter
// engine
func userStreamHeader(udp bool, port int) [3]byte                    // always 3 bytes
```

- [ ] **گام ۱: آزمون‌های شکست‌خورده**
  ```go
  func TestSynPayloadDelivered(t *testing.T)   // OpenStreamSyn(EncodeSyn(0xA1B2C3D4, 1)) ⇒ طرف دیگر Syn() == (0xA1B2C3D4, 1)
  func TestBadSynRefused(t *testing.T)         // SYN با بار 0 و 12 بایت ⇒ به AcceptStream نمی‌رسد، FIN برمی‌گردد، BadSyn()==2
  func TestSynPayloadBeforeData(t *testing.T)  // داده‌ای که بلافاصله پس از Open نوشته شود، پس از SYN با بار می‌رسد
  func TestUserStreamHeaderAlways3(t *testing.T) // برای kindTCP و kindUDP و هر درگاه: len==3، [kind][port BE]
  ```
- [ ] **گام ۲:** اجرا: `go test ./third_party/smux/ ./engine/ -run 'Syn|UserStreamHeader' -count=1`. انتظار: شکست.
- [ ] **گام ۳:** پیاده‌سازی:
  - SYN بار ۱۳ بایتی را در `frame.data` می‌برد.
  - گیرنده طول را می‌سنجد. جریان با بار نادرست ساخته نمی‌شود: FIN همان sid فرستاده می‌شود و `BadSyn` بالا می‌رود.
  - مسیر سرآیند یک‌بایتی حذف می‌شود.
  - جریان‌های کنترلی (`kindCtrl`، `kindStats`، `kindInfo`، `kindPool`، `kindL3`) با `EncodeSyn(0, 0)` باز می‌شوند.
  - سطر وضعیت شمارندهٔ `bad syn` را دارد.
- [ ] **گام ۴:** همان فرمان، و سپس `go test ./third_party/smux/ ./engine/ -count=1`. انتظار: سبز.
- [ ] **گام ۵:** ثبت: `سیم ۳: بار SYN سیزده‌بایتی با برچسب کاربر و اشارهٔ کلاس؛ سرآیند جریان فقط سه‌بایتی`.

### کار ۷: توصیف‌گر ۱۵ بایتی TUN و کلید پنج‌تایی IPv6

**فایل‌ها:**
- تغییر: `hs2-src/tlscarrier/carrier.go` (`AppendFrame`/`readFrame` برای نوع TUN)، `hs2-src/engine/l3_link.go` (`flowHash`، و قاب‌بندی `kindL3`).
- آزمون: `hs2-src/engine/l3_v3_test.go`، `hs2-src/tlscarrier/tundesc_test.go`

**رابط‌ها (فرآورده):**
```go
// tlscarrier
const tunDescLen = 15
func AppendTunFrame(dst []byte, ftype byte, userTag, flow uint32, pkt []byte) []byte
func ParseTunDesc(h []byte) (ftype byte, n int, userTag, flow uint32, err error)
// engine
func flowKey(pkt []byte, toUser bool) (flow uint32, userIP netip.Addr, ok bool) // پنج‌تایی v4 و v6؛ userIP = نشانی سمت کاربر
```

- [ ] **گام ۱: آزمون‌های شکست‌خورده**
  ```go
  func TestTunDescRoundTrip(t *testing.T)        // AppendTunFrame ⇒ ParseTunDesc همان مقدارها؛ len(header)==15
  func TestTunDescOldRejected(t *testing.T)      // توصیف‌گر ۷ بایتی قدیم ⇒ err != nil
  func TestFlowKeyIPv6FiveTuple(t *testing.T)    // دو جریان TCP میان دو میزبان IPv6 با درگاه متفاوت ⇒ flow متفاوت
  func TestFlowKeyIPv6ExtBound(t *testing.T)     // ۹ سرآیند الحاقی یا بیش از ۲۵۶ بایت ⇒ کلید سه‌تایی (همان flow برای درگاه‌های متفاوت)
  func TestFlowKeyUserSide(t *testing.T)         // toUser=false ⇒ userIP = مبدأ؛ toUser=true ⇒ userIP = مقصد
  func FuzzParseTunDesc(f *testing.F)
  func FuzzFlowKey(f *testing.F)
  ```
- [ ] **گام ۲:** اجرا: `go test ./tlscarrier/ ./engine/ -run 'TunDesc|FlowKey' -count=1`. انتظار: شکست.
- [ ] **گام ۳:** پیاده‌سازی طبق §۴.۳ و §۳.۱. `flow hash` همان FNV `flowHash` است، با گسترش به پنج‌تایی IPv6.
- [ ] **گام ۴:** همان فرمان و هر دو fuzz هر کدام ۶۰s. انتظار: سبز.
- [ ] **گام ۵:** ثبت: `سیم ۳: توصیف‌گر ۱۵ بایتی TUN با برچسب کاربر و هش جریان؛ کلید پنج‌تایی IPv6`.

### کار ۸: C1، رده‌بند و برچسب کاربر

**فایل‌ها:**
- تازه: `hs2-src/engine/classifier.go`
- آزمون: `hs2-src/engine/classifier_test.go`

**رابط‌ها (فرآورده):**
```go
type Class uint8
const (ClassUnknown Class = iota; ClassLight; ClassBulk)
func UserTag(ip netip.Addr, v6Prefix int) uint32          // v4: FNV نشانی؛ v6: FNV پیشوند v6Prefix (پیش‌فرض 64)
type flowStats struct{ /* atomic counters per direction */ }
func (f *flowStats) Add(tx bool, n int)                     // مسیر داغ: یک جمع atomic
type flowClassifier struct{ /* state per flow */ }
func (c *flowClassifier) Tick(f *flowStats, now time.Duration) (classTx, classFlow Class, confirmedLight bool)
func ClassOff() bool                                         // HS2_TUNE_CLASS=0 ⇒ همیشه ClassBulk
```

- [ ] **گام ۱: آزمون‌های شکست‌خورده** (زمان مصنوعی، یک تیک = ۱۰۰ms)
  ```go
  func TestClassifierDownload(t *testing.T)     // 1 MiB/s پیوسته: از light بودن بیش از 32 KiB + یک تیک نمی‌گذرد
  func TestClassifierHTTP(t *testing.T)         // درخواست 600 B و پاسخ 8 KiB هر 1s ⇒ همیشه light، confirmedLight از تیک دوم
  func TestClassifierBurst20KiB(t *testing.T)   // انفجار 20 KiB در 200ms ⇒ bulk؛ پس از 2s سکوت ⇒ light در یک تیک
  func TestClassifierVoIPTun(t *testing.T)      // 50 بستهٔ 160 B در ثانیه ⇒ light
  func TestClassifierPeriodic(t *testing.T)     // 1s روشن با 1 MiB/s، 2s خاموش ⇒ حکم‌ها طبق قاعده؛ هر گذار ≤ یک تیک دیر
  func TestClassifierUnknownFirst32K(t *testing.T) // 32 KiB نخست جریان تازه unknown و در ردهٔ صف مثل light
  func TestUserTagIPv6Slash64(t *testing.T)     // سه نشانی در یک /64 ⇒ یک برچسب؛ دو /64 ⇒ دو برچسب
  func TestClassOffAllBulk(t *testing.T)        // HS2_TUNE_CLASS=0 ⇒ ClassBulk برای همه
  func TestClassifierReplay(t *testing.T)       // testdata/c1_replay.jsonl.gz (کار ۳): نرخ رده‌بندی نادرست گزارش؛ هیچ جریان حجیم بیش از 32 KiB + یک تیک در light
  func BenchmarkClassifierTick(b *testing.B)    // 10 و 10000 جریان: هزینهٔ هر جریان ثابت
  ```
- [ ] **گام ۲:** اجرا: `go test ./engine/ -run 'Classifier|UserTag|ClassOff' -count=1`. انتظار: شکست.
- [ ] **گام ۳:** پیاده‌سازی طبق قاعده‌های §۳.۱ (قیدهای سراسری). سطر وضعیت `classes: light N / bulk M / unknown K per link; flips/min: light→bulk a, bulk→light b`.
- [ ] **گام ۴:** همان فرمان و benchmark. انتظار: سبز، و هزینهٔ هر جریان در ۱۰ و ۱۰۰۰۰ جریان در یک مرتبه.
- [ ] **گام ۵:** ثبت: `C1: رده‌بند و برچسب کاربر با آزمون مدل و بازپخش ترافیک واقعی`.

### کار ۹: C2، هستهٔ خالص زمان‌بند

**فایل‌ها:**
- تازه: `hs2-src/engine/sched.go` (ژنریک و مستقل از smux و TUN، تا هر دو زاویه به کارش ببرند)
- آزمون: `hs2-src/engine/sched_test.go`

**رابط‌ها:**
- **مصرف:** `Class` از کار ۸.
- **فرآورده:**
```go
type Lane uint8
const (LaneCtrl Lane = iota; LaneLatency; LaneBulk)
type Sched[T any] struct{ /* ... */ }
func NewSched[T any]() *Sched[T]
func (s *Sched[T]) Push(it T, user uint32, flow uint64, lane Lane, size int)
func (s *Sched[T]) Next() (it T, ok bool)                       // O(1)
func (s *Sched[T]) Tick(now time.Duration, linkRate float64)    // هر 100ms: کف‌ها، سطل‌ها، تقاضا
func (s *Sched[T]) Stats() SchedStats
func BulkFrameSize(rate float64) int                            // clamp(rate*0.005, 1024, 16384); rate به بایت بر ثانیه
const LaneFrameMax = 2048
func SchedOff() bool                                            // HS2_TUNE_SCHED=0
```

- [ ] **گام ۱: آزمون‌های شکست‌خورده** (سوکت مصنوعی با نرخ R)
  ```go
  func TestSchedLightWait(t *testing.T)            // انتظار هر فریم سبک ≤ BulkFrameSize(R)/R + فریم در حال نوشتن
  func TestSchedLoneUser(t *testing.T)             // کاربر تنها با 1 و با 16 جریان ⇒ ≥ 0.95×R
  func TestSchedOneVsSixteen(t *testing.T)         // کاربر 1 جریانه ≥ 0.9×min(D_u, R/2)؛ کاربر 16 جریانه باقی را می‌گیرد؛ جمع ≥ 0.97×R
  func TestSchedLowDemandFreesShare(t *testing.T)  // کاربری با تقاضای 0.1×R فقط همان را می‌گیرد، بقیه پخش می‌شود
  func TestSchedNewHeavyUser(t *testing.T)         // ورود کاربر سنگین تازه: p99 سبک و کف کاربر حجیم موجود از کران بیرون نمی‌رود
  func TestSchedCtrlNeverBehindData(t *testing.T)  // فریم LaneCtrl همیشه نفر بعدی
  func TestSchedLaneCredit(t *testing.T)           // کاربری که بیش از 160 KiB/s با انفجار 32 KiB سبک می‌فرستد ⇒ اضافه در حلقهٔ حجیم خودش
  func TestSchedThousandStreamsUser(t *testing.T)  // کاربر با 1000 جریان کنار کاربر 1 جریانه ⇒ کف دومی ≥ 0.9×؛ کنترل بی‌انتظار
  func TestBulkFrameSize(t *testing.T)             // 1 Mbit/s⇒1024 (کف)؛ 10 Mbit/s⇒6250؛ 26 Mbit/s⇒16250؛ 27 Mbit/s و بیشتر⇒16384 (سقف)
  func BenchmarkSchedNext(b *testing.B)            // 10 و 10000 جریان: ns/op ثابت (نسبت < 2)
  ```
- [ ] **گام ۲:** اجرا: `go test ./engine/ -run 'Sched|BulkFrameSize' -count=1`. انتظار: شکست.
- [ ] **گام ۳:** پیاده‌سازی DRR دوسطحی طبق §۳.۲:
  - فهرست پیوندی کاربران فعال.
  - برای هر کاربر، حلقهٔ جریان‌هایش.
  - نقشهٔ `userTag → رکورد`.
  - کف‌ها در `Tick` حساب می‌شوند، نه در `Next`.
  - مقدار آغازین سطل توکن و صف‌ها از قیدهای سراسری می‌آید.
  - کران ۶۴ فریم معلق کاربر و صف انتظار FIFO جریان شصت‌وپنجم کار لایهٔ یکپارچه‌سازی (کارهای ۱۵ و ۱۶) است. `Sched` فقط `Stats` آن را می‌دهد.
- [ ] **گام ۴:** همان فرمان و benchmark. انتظار: سبز.
- [ ] **گام ۵:** ثبت: `C2: هستهٔ خالص زمان‌بند با کف بر پایهٔ تقاضا و مازاد به ازای جریان`.

### کار ۱۰: C3، نگهبان صف میزبان

**فایل‌ها:**
- تازه: `hs2-src/engine/hostq.go` (محاسبهٔ خالص)، `hs2-src/engine/hostq_linux.go` (`setsockopt`/`ioctl`)، `hs2-src/engine/hostq_other.go`
- آزمون: `hs2-src/engine/hostq_test.go`

**رابط‌ها (فرآورده):**
```go
func Lowat(rate float64, mss int) int                       // clamp(rate*0.002, max(2*mss, 4096), 262144)
type hostGate struct{ /* hysteresis state */ }
func (g *hostGate) Update(rate float64, mss int) (on bool, cap, lowat int)
func (g *hostGate) Room(notsent int) int                    // cap - notsent، کف 0
type hostQ struct{ /* per link */ }
func newHostQ(c net.Conn, sys hostSys) *hostQ               // hostSys: setLowat/notsent؛ ساختگی در آزمون
func (h *hostQ) Write(p []byte) (int, error)                // یک فریم ورای کم‌آب؛ در دروازه تا Room
func HostQOff() bool                                        // HS2_TUNE_HQG=0
```

- [ ] **گام ۱: آزمون‌های شکست‌خورده**
  ```go
  func TestLowatTable(t *testing.T)        // MSS 1448: 1→4096، 16→4096، 64→16000، 100→25000، 1000→250000 Mbit/s
  func TestGateCap(t *testing.T)           // 1 Mbit/s⇒cap 2896؛ 20 Mbit/s⇒5000؛ lowat دروازه = max(cap/2, MSS)
  func TestGateHysteresis(t *testing.T)    // نرخ نوسانی 22..28 Mbit/s ⇒ بی پرش؛ زیر 20 روشن، بالای 30 خاموش
  func TestHostWaitBound(t *testing.T)     // برای R در {1,5,10,25,100,1000}: فریم در حال نوشتن + نفرستاده (قاعدهٔ skb) ≤ max(20ms, 3*1500B/R)
  func TestLowatAdapts(t *testing.T)       // افت ناگهانی 10× ⇒ lowat و cap در ≤ 500ms تازه
  func TestLowatWriteOnChange(t *testing.T) // تغییر < 25٪ ⇒ setLowat صدا زده نمی‌شود
  func TestHostQNoSyscalls(t *testing.T)   // hostSys که ENOPROTOOPT می‌دهد ⇒ رفتار امروز (کم‌آب ثابت + sendGate) و یک خط لاگ
  ```
- [ ] **گام ۲:** اجرا: `go test ./engine/ -run 'Lowat|Gate|HostWait|HostQ' -count=1`. انتظار: شکست.
- [ ] **گام ۳:** پیاده‌سازی طبق §۳.۳:
  - خواب در netpoller، بی `poll(2)`.
  - `sendGate` پشت `HS2_TUNE_HQG=0` می‌ماند.
  - سطر وضعیت `hostq link N: lowat K KiB, gate on|off (cap C B), notsent p95 B, blocked ratio r`.
- [ ] **گام ۴:** همان فرمان. انتظار: سبز.
- [ ] **گام ۵:** ثبت: `C3: نگهبان صف میزبان با کم‌آب پویا و دروازهٔ نوشتن زیر ۲۵ Mbit/s`.

### کار ۱۱: C4-lite، سقف pacing در هر دو سر

**فایل‌ها:**
- تازه: `hs2-src/engine/sendcap.go`
- تغییر: `hs2-src/engine/health_linux.go` (خواندن `tcpi_delivery_rate_app_limited` و `tcpi_snd_mss`)، `hs2-src/engine/pacenet.go` (سقف خودش را به C4-lite می‌دهد)، `hs2-src/engine/pacecap_linux.go`
- آزمون: `hs2-src/engine/sendcap_test.go`

**رابط‌ها (فرآورده):**
```go
type capSample struct{ Now time.Duration; Delivery float64; AppLimited bool; Acked, Written uint64; Segs uint32 }
type sendCap struct{ /* 2s max filter, state */ }
func (c *sendCap) Update(s capSample) (rate uint64, apply, lift bool)  // 1.25×max(delivery 2s)، کف 1<<20 B/s
func (c *sendCap) SetPacenetCap(rate uint64)                           // کمینهٔ دو سقف اعمال می‌شود
func SendCapOff() bool                                                 // HS2_TUNE_SENDCAP=0
```

- [ ] **گام ۱: آزمون‌های شکست‌خورده**
  ```go
  func TestSendCapFloor(t *testing.T)            // delivery 100 KiB/s ⇒ rate == 1<<20
  func TestSendCapAfterNonAppLimited(t *testing.T) // پیش از نخستین نمونهٔ AppLimited=false ⇒ apply=false
  func TestSendCapLiftOnIdle(t *testing.T)       // کمتر از 8 KiB و کمتر از 8 قطعه در بازهٔ بیکاری ⇒ lift=true
  func TestSendCapMinWithPacenet(t *testing.T)   // SetPacenetCap کوچک‌تر ⇒ همان اعمال می‌شود
  func TestSendCapModelNoOvershoot(t *testing.T) // مدل فرستندهٔ BBR (wcore_model_test.go)، دو جهت، 4 دانلود + 2 آپلود + کاوشگر:
                                                 // در هیچ RTT پس از نخستین RTT غیر app-limited، نرخ ارسال > 1.3×گلوگاه نمی‌شود
  func TestSendCapNoDeliveryRate(t *testing.T)   // هسته‌ای بی tcpi_delivery_rate ⇒ هیچ سقفی و یک خط لاگ
  ```
- [ ] **گام ۲:** اجرا: `go test ./engine/ -run 'SendCap' -count=1`. انتظار: شکست.
- [ ] **گام ۳:** پیاده‌سازی طبق §۳.۴:
  - قاعدهٔ «سوکت ساکت» همان `pacenet.go:152-184` است.
  - سقف روی همهٔ سوکت‌های لینک در هر دو سر اعمال می‌شود.
  - هستهٔ پنجره و تور pacing روی خروجی روشن می‌مانند.
  - سطر وضعیت `sendcap link N: cap C Mbit/s (1.25 x max delivery 2s), app-limited a%, idle-lifted n`.
- [ ] **گام ۴:** همان فرمان، و سپس `go test ./engine/ -run 'Wcore' -count=1`. انتظار: سبز، بی پسرفت مدل Z5.
- [ ] **گام ۵:** ثبت: `C4-lite: سقف pacing متقارن ۱٫۲۵× نرخ تحویل روی همهٔ لینک‌ها`.

### کار ۱۲: PROXY protocol روی درگاه‌های `proxy`

**فایل‌ها:**
- تازه: `hs2-src/engine/proxyproto.go`
- تغییر: `hs2-src/engine/stream_iran.go` (`serveUserTCP`)، `hs2-src/cmd/hs2/main.go` و `hs2-src/cmd/hs2/check.go` (پرچم‌های `proxy` و `proxy_from` در `port_map`)
- آزمون: `hs2-src/engine/proxyproto_test.go`

**رابط‌ها (فرآورده):**
```go
func readProxyHeader(r *bufio.Reader) (src netip.AddrPort, err error)   // v1 حداکثر 107 B؛ v2 حداکثر 16+216 B
func proxyAllowed(from netip.Addr, allow []netip.Prefix) bool           // پیش‌فرض: 127.0.0.0/8 و ::1/128
```

- [ ] **گام ۱: آزمون‌های شکست‌خورده**
  ```go
  func TestProxyV1(t *testing.T)            // "PROXY TCP4 …", "PROXY TCP6 …" ⇒ src درست؛ "PROXY UNKNOWN" ⇒ نشانی خود اتصال
  func TestProxyV2(t *testing.T)            // PROXY و LOCAL، IPv4 و IPv6
  func TestProxyTooLong(t *testing.T)       // سرآیند v1 بیش از 107 B ⇒ خطا
  func TestProxyNoHeaderClosed(t *testing.T) // روی درگاه proxy، اتصال بی سرآیند ⇒ بسته می‌شود
  func TestProxyFromNotAllowed(t *testing.T) // سرآیند از نشانی بیرون از proxy_from ⇒ بسته می‌شود
  func FuzzProxyHeader(f *testing.F)
  ```
- [ ] **گام ۲:** اجرا: `go test ./engine/ -run 'Proxy' -count=1`. انتظار: شکست.
- [ ] **گام ۳:** پیاده‌سازی. برچسب کاربر از نشانی واقعی کلاینت ساخته می‌شود (`UserTag`، کار ۸).
- [ ] **گام ۴:** همان فرمان و fuzz به مدت ۶۰s. انتظار: سبز.
- [ ] **گام ۵:** ثبت: `PROXY protocol نسخهٔ ۱ و ۲ روی درگاه‌های proxy با proxy_from`.

### کار ۱۳: C15، `hs2 link` و منوی نصب‌کننده

**فایل‌ها:**
- تازه: `hs2-src/cmd/hs2/link.go`، `hs2-src/cmd/hs2/link_test.go`، `scripts/install_link_test.sh`
- تغییر: `hs2-src/cmd/hs2/main.go` (زیرفرمان `link`)، `hs2-src/cmd/hs2/check.go` (کلید `link`)، `install.sh` (`show_link`، `parse_link`، بلوک `link` در هر دو سر، گزینهٔ منوی اصلی `Show a tunnel's hs2:// link`، و `l) Show the hs2:// link` در منوی هر تونل)

**رابط‌ها (فرآورده):**
```go
type linkFields struct{ Endpoint, Domain, Shared, Panel, Carrier, UDP, Transport, Direction, MTU, Encap, Proto, Unit, TunBase string }
func encodeLink(f linkFields) (string, error)            // "hs2://" + base64.StdEncoding("f1|…|f13")؛ فیلد با '|' یا پایان سطر ⇒ خطا
func decodeLink(s string) (linkFields, error)
func buildLink(cfg map[string]any, unit string, ov linkFields) (link string, notes, missing []string)
// CLI: hs2 link -c CFG [--endpoint ..] [--domain ..] [--transport tcp|tun] [--udp true|false] ; hs2 link --list ; hs2 link --decode LINK
// exit: 0 ok, 3 missing (names on stderr), 2 bad config, 4 no read access
```

- [ ] **گام ۱: آزمون‌های شکست‌خورده**
  ```go
  func TestLinkRoundTripAll(t *testing.T)  // همهٔ ترکیب‌ها (§۳.۱۶ آزمون ۱)، مستقیم و معکوس، هر دو سمت: link → decode → cfg → buildLink == link
  func TestLinkGoldenBash(t *testing.T)    // encodeLink == خروجی encode_link استخراج‌شده از install.sh با bash، برای همان فیلدها
  func TestLinkRefuseEndpoint(t *testing.T) // شنوندهٔ 0.0.0.0 بی بلوک link ⇒ missing==["endpoint"]، کد 3
  func TestLinkRefuseTransport(t *testing.T) // l3mtcp بی link.transport ⇒ missing==["transport"]
  func TestLinkRejectPipe(t *testing.T)    // فیلدی با '|' ⇒ خطا
  func TestLinkNoNetworkNoLog(t *testing.T) // در فضای نام شبکهٔ خالی کار می‌کند؛ هیچ فایل تازه و هیچ خط لاگی لینک را ندارد؛ مجوز فایل‌ها بی‌تغییر
  func FuzzDecodeLink(f *testing.F)
  ```
  ```bash
  # scripts/install_link_test.sh: منوی غیرتعاملی با ورودی اسکریپتی:
  # فهرست ← انتخاب ← پرسش فیلد کم ← ذخیرهٔ اختیاری در بلوک link (0600، .prev)،
  # با و بی qrencode، و امتناع بی root با پیام روشن
  ```
- [ ] **گام ۲:** اجرا: `go test ./cmd/hs2/ -run 'Link' -count=1 && bash ../scripts/install_link_test.sh`. انتظار: شکست.
- [ ] **گام ۳:** پیاده‌سازی طبق §۳.۱۶:
  - ترتیب یافتن هر فیلد: بلوک `link`، سپس فیلدهای کاری، سپس استنتاج، وگرنه امتناع.
  - پیام‌های خطای استاندارد همان متن‌های سند.
  - اگر باینری `hs2 link` نداشت، نصب‌کننده به `encode_link` برمی‌گردد.
- [ ] **گام ۴:** همان فرمان، و fuzz به مدت ۶۰s. سپس `hs2 link -c` را روی پیکربندی هر دو سرور میدانی اجرا کن (فقط‌خواندنی). انتظار: لینک با لینک نصب برابر است، یا فیلدهای کم با نام گفته می‌شوند. خود لینک در هیچ گزارشی نمی‌آید.
- [ ] **گام ۵:** ثبت: `C15: بازیابی لینک hs2:// از پیکربندی، با فهرست تونل‌ها و منوی نصب‌کننده`.

### کار ۱۴: `hs2-safe-apply` در مخزن و `READY=1`

**فایل‌ها:**
- تازه: `scripts/hs2-safe-apply` (از نسخهٔ نصب‌شده در `/usr/local/sbin/` روی خارج شروع کن)، `scripts/hs2-safe-apply_test.sh`
- تغییر: `hs2-src/cmd/hs2/main.go` (`sdNotifyReady()`)، `install.sh` (نصب از مخزن)

**رابط‌ها (فرآورده):**
- `HS2_SAFE_UNIT`: واحد.
- `HS2_SAFE_PEER_WAIT`: ثانیه، پیش‌فرض ۳۰۰.
- نتیجه در `/root/hs2-safe-apply.last`.
- `func sdNotifyReady()`: یک پیام `READY=1` روی `$NOTIFY_SOCKET`؛ بی آن هیچ کاری نمی‌کند.

- [ ] **گام ۱: آزمون‌های شکست‌خورده** (`systemctl` و `ping` ساختگی در `PATH`)
  - `ok`: hs2 بالا آمد و همتا پاسخ داد، پس نتیجه OK است.
  - `not-up`: hs2 تا ۶۰s پایدار نماند، پس باینری، پیکربندی و drop-inها برمی‌گردند.
  - `no-peer`: همتا تا `HS2_SAFE_PEER_WAIT` پاسخ نداد، پس بازگشت.
  - `peer-late`: همتا پس از ۲۰۰s پاسخ داد و `HS2_SAFE_PEER_WAIT=400` است، پس می‌ماند.
  - `bad-config`: `hs2 check` رد کرد، پس هیچ تغییری اعمال نمی‌شود.
  - `dropins`: پوشهٔ `<unit>.service.d/` پس از بازگشت دقیقاً همان قبلی است.
  - `TestSdNotifyReady` (Go): سوکت unixgram ساختگی دقیقاً `READY=1` می‌گیرد؛ بی `NOTIFY_SOCKET` هیچ خطایی نیست.
- [ ] **گام ۲:** اجرا: `bash scripts/hs2-safe-apply_test.sh && cd hs2-src && go test ./cmd/hs2/ -run SdNotify -count=1`. انتظار: شکست.
- [ ] **گام ۳:** پیاده‌سازی طبق §۵.۱ («بازگشت‌پذیری»). نشانی پینگ از `peer_ip` پیکربندی خوانده می‌شود.
- [ ] **گام ۴:** همان فرمان. انتظار: سبز.
- [ ] **گام ۵:** ثبت: `hs2-safe-apply در مخزن با آزمون‌هایش؛ READY=1 روی NOTIFY_SOCKET`.

### کار ۱۵: نمونهٔ زاویهٔ الف، زمان‌بند درون smux

**مالک:** زیرعامل جدا، بی دیدن کار ۱۶.

**رابط‌ها:**
- **مصرف:** همهٔ فرآورده‌های کارهای ۵ تا ۱۱.
- **فرآورده:** شاخهٔ `r1-angle-a`.

- [ ] **گام ۱:** پیاده‌سازی:
  - `Sched` جای heap و `shaperLoop` در `third_party/smux/session.go` می‌نشیند.
  - smux `(userTag, class)` را با تابع بازخوانی از موتور می‌گیرد (`Config.FlowInfo func(sid uint32) (uint32, engine.Class)`، یا معادلی که چرخهٔ import نسازد).
  - `writeV2` اندازهٔ فریم را از `BulkFrameSize` و `LaneFrameMax` می‌گیرد.
  - `sendLoop` از راه `hostQ` (کار ۱۰) می‌نویسد.
  - TUN: صف هر جریان (۶۴ بسته، ۲۰ms، ۱۰۲۴ برای هر کاربر) و جدول جریان پین‌شده در هر دو سر. `kindL3` از همان `Sched` تغذیه می‌شود. FIFO و `l3MaxSojourn` پشت `HS2_TUNE_SCHED=0` می‌روند.
  - کف ۶۴ فریم معلق مسیر جریان برای هر کاربر، با صف انتظار FIFO.
  - سقف درخواست heap (۱۰۲۴) دیگر نیست.
  - حساب صف‌های C2 و صف‌های جریان TUN هر کاربر با `user_mem_share` (کف + قرض)، و سطر «کاربران بالای سهم حافظه».
  - سطر وضعیت `sched link N: lane p50/p99 a/b ms, bulk frame K KiB, users U (active A), drops light/bulk x/y, blocked-writers z, users at memory cap m`.
- [ ] **گام ۲:** آزمون‌های یکپارچه:
  ```go
  func TestAngleSameLinkProbe(t *testing.T) // دو نشست smux روی pipe با نرخ 10 Mbit/s: 4 دانلود + 2 آپلود + کاوشگر ⇒ p99 کاوشگر ≤ RTT + 20ms
  func TestAngleP10(t *testing.T)           // lone16، one_vs_16، newheavy روی همان pipe
  func TestAngleSchedOff(t *testing.T)      // HS2_TUNE_SCHED=0 ⇒ رفتار و آزمون‌های قدیم سبز
  ```
- [ ] **گام ۳:** `go test ./... -count=1` و `go test -race ./third_party/smux/ ./engine/ -timeout 20m`. انتظار: سبز.
- [ ] **گام ۴:** ثبت روی `r1-angle-a`.

### کار ۱۶: نمونهٔ زاویهٔ ب، زمان‌بند در موتور

**مالک:** زیرعامل جدا، بی دیدن کار ۱۵.

**رابط‌ها:**
- **مصرف:** همان کارهای ۵ تا ۱۱.
- **فرآورده:** شاخهٔ `r1-angle-b`.

- [ ] **گام ۱:** پیاده‌سازی:
  - smux فقط فریم می‌سازد و هر فریم را به یک صف موتور تحویل می‌دهد.
  - `sendLoop` از `Sched` موتور می‌خواند. بقیهٔ کار مثل گام ۱ کار ۱۵ است: TUN، کران‌ها و `hostQ`.
  - زیرعامل باید مرز تازهٔ smux و موتور را در یک بند README شاخه بنویسد، چون R7 و TUN روی همین مرز ساخته می‌شوند.
- [ ] **گام ۲:** همان سه آزمون کار ۱۵، با نام‌های خودش.
- [ ] **گام ۳:** همان فرمان‌های گام ۳ کار ۱۵.
- [ ] **گام ۴:** ثبت روی `r1-angle-b`. سپس ثبتی روی `redesign-r1` با عنوان `R1 گام 2: بلوک‌های مشترک و دو نمونهٔ زاویهٔ الف و ب`، که دو شاخه را در `docs/architecture/wip/r1/prototypes.fa.md` نام می‌برد.

---

## گام ۳ روش: داوری روی چیدمان

### کار ۱۷: داوری

**مالک:** زیرعامل داور، جدا از سازندگان.

- [ ] **گام ۱:** سه باینری بساز: پایه، الف و ب.
- [ ] **گام ۲:** همهٔ خانه‌های §۵.۱ را روی `rig-scale` اجرا کن:
  - N ∈ {۱، ۴، ۱۶، ۶۴، ۱۲۸، ۳۰۰} و سقف میزبان
  - هر خانه با C4-lite روشن و خاموش
  - ABBABA با ۳ تکرار
  - خانهٔ «زمان تا نخستین ۱، ۴ و ۱۶ MiB» پس از بیکاری
  - خانه‌های پس‌نزدن Z3b، ماتریس خوانندهٔ کند Z/Z2، ۱۶ کاربر تعاملی روی ۸ لینک، و TUN با تغییر اندازهٔ استخر
  - ۲۰۰ کاربر پشت یک پراکسی محلی با PROXY protocol (باید ۲۰۰ برچسب دیده شود، نه یک)
  - کلاینت‌های IPv6 با چند نشانی چرخان در یک /64 (باید یک کاربر شمرده شوند)، و پینگ ICMPv6 کنار جریان حجیم TUN IPv6 همان میزبان
  - توزیع طول رکوردهای TLS روی سیم، قدیم در برابر تازه، با `obfs/dpi_eval.py` (شکل روی سیم نباید عوض شود)
- [ ] **گام ۳:** نتیجه را در `docs/architecture/wip/r1/judge.fa.md` بنویس:
  - جدول هر خانه
  - حکم P10 (الف تا هـ)
  - p99 کاوشگر و پینگ TUN
  - توان در برابر پایه
  - پردازنده (فقط گزارش)
  - طرح برگزیده: الف، ب، ترکیب، یا «هیچ کاری»، با دلیل عددی
  - سه پرسش باز «پیش از R1» از §۱۰ سند، با عدد: کافی بودن C4-lite برای معیار همان‌لینک، خانهٔ تنگ ۱ Mbit/s، و کندی رشد پس از بیکاری
- [ ] **گام ۴:** طرح برگزیده را روی `redesign-r1` ادغام کن و ثبت بزن: `R1 گام 3: داوری؛ طرح برگزیده …`.

## گام ۴ روش: تیم قرمز

### کار ۱۸: تیم قرمز

**مالک:** زیرعامل جدا، در چند دور تا وقتی یافتهٔ جدی تازه‌ای نماند.

- [ ] **گام ۱:** حمله به طرح برگزیده با این موضوع‌ها:
  - گرسنگی هر رده
  - دور زدن اعتبار خط تأخیر
  - هزاران جریان در هر کاربر
  - کاربر پشت NAT
  - سرریز TUN
  - نوسان نرخ حول ۲۵ Mbit/s
  - هستهٔ بی‌قابلیت
  - SYN و توصیف‌گر و سرآیند PROXY نادرست
  - همتای v2
  - نصب ناهم‌زمان دو سر
  - ۳۰۰ لینک × تیک‌ها
- [ ] **گام ۲:** هر یافتهٔ واقعی با یک آزمون بازتولید و سپس اصلاح می‌شود. برای هر دور یک ثبت بزن.
- [ ] **گام ۳:** گزارش دورها را در `docs/architecture/wip/r1/redteam.fa.md` بنویس و ثبت بزن: `R1 گام 4: تیم قرمز؛ N دور، M یافته`.

## گام ۵ روش: ادغام و آزمون کامل

### کار ۱۹: آزمون کامل

- [ ] **گام ۱:** این‌ها را اجرا کن:
  - `go test ./... -count=1`
  - `go test -race` روی بسته‌های تغییرکرده، با `-timeout 20m` برای موتور
  - ساخت 386، arm7 و arm64
  - `GOARCH=386 go test` روی بسته‌های تغییرکرده
  - arm7 با qemu-user
  - هر fuzz به مدت ۶۰s
- [ ] **گام ۲:** آزمون‌های لرزان `flaky.fa.md` (کار ۱): هر کدام رفع یا با دلیل نوشته قرنطینه شود.
- [ ] **گام ۳:** ماتریس هستهٔ لینوکس:
  - خانه‌های اصلی چیدمان روی یک ماشین مجازی با هستهٔ ۵٫۱۰ (qemu روی خارج؛ اگر KVM نیست، TCG با نرخ‌های پایین) و روی هستهٔ ۶٫x خود خارج.
  - اگر ماشین مجازی ساخته نشد، علت و راه جایگزین در گزارش نوشته می‌شود و این شکست رد نمی‌شود.
- [ ] **گام ۴:** پس‌نزدن معیارهای Z تا Z5. همهٔ آزمون‌های موجود سبز بمانند.
- [ ] **گام ۵:** ثبت بزن: `R1 گام 5: ادغام و آزمون کامل؛ …`.

## گام ۶ روش: بازبینی مستقل

### کار ۲۰: بازبینی مستقل

- [ ] **گام ۱:** زیرعامل بازبین تازه کل تفاوت `redesign-r1` با `b780b63` را بازبینی کند. هر یافته باید با راستی‌آزمایی تقابلی تأیید شود.
- [ ] **گام ۲:** اصلاح‌ها هر کدام با یک آزمون. سپس ثبت بزن: `R1 گام 6: بازبینی مستقل؛ …`.

## گام ۷ روش: باینری ایستا و نصب

### کار ۲۱: باینری

- [ ] **گام ۱:** `CGO_ENABLED=0 go build`، `sha256`، و خط Release در `hs2-src/VALIDATION.md`.
- [ ] **گام ۲:** نصب با `hs2-safe-apply`، اول خارج و سپس ایران، با `HS2_SAFE_PEER_WAIT` کافی (تمرکز بازبینی ۱).
- [ ] **گام ۳:** وارسی:
  - `hs2 status` روی هر دو سر `protocol: v3 level 1` را نشان می‌دهد.
  - `refused v2 peers` و `bad syn` صفرند.
- [ ] **گام ۴:** ثبت بزن: `R1 گام 7: باینری ایستا build …`.

## گام ۸ روش: سنجش میدانی و ماندگاری

### کار ۲۲: میدان

- [ ] **گام ۱:** S1 تا S5، و S5 با دو IP منبع، به‌صورت ABBABA با ۳ تکرار. در هر نوبت هر دو سرور با هم عوض می‌شوند، با `hs2-safe-apply` و اول خارج. هیچ ساخت یا آزمونی روی خارج در حین اجرا انجام نمی‌شود.
- [ ] **گام ۲:** خانهٔ «بازگشت»: نصب باینری تازه، بازگشت اجباری، و برگشتن تونل در ≤ ۵ دقیقه.
- [ ] **گام ۳:** پایش **حداکثر یک ساعت** در برابر همان ساعت‌های پایه. ماندگاری شتاب‌یافته پیش از میدان روی چیدمان انجام شده است: یک ساعت چرخش سنگین (صدها کاربر با باز و بسته کردن پیاپی اتصال، کشتن و بازسازی لینک‌ها، تغییر اندازهٔ استخر)، نمونهٔ دقیقه‌ای heap، RSS و گوروتین هر دو پردازه؛ پذیرش: رشد heap نیم‌ساعت آخر < ۵٪ و بازگشت به سطح پیش از بار.
- [ ] **گام ۴:** نتیجه را در `docs/architecture/wip/r1/field.fa.md` بنویس و ثبت بزن: `R1 گام 8: سنجش میدانی، پایش یک‌ساعته و ماندگاری شتاب‌یافته`.

## گام ۹ روش: مستندات

### کار ۲۳

- [ ] **گام ۱:** این‌ها را بنویس و به‌روز کن:
  - بخش R1 در `CHANGELOG.md` (انگلیسی)
  - یادداشت بالای `ARCHITECTURE.fa.md`
  - وضعیت R1 در `REDESIGN.fa.md` و `ROADMAP.fa.md`
- [ ] **گام ۲:** ثبت بزن: `R1 گام 9: مستندات`.

## گام ۱۰ روش: گزارش قدیم در برابر تازه

### کار ۲۴

- [ ] **گام ۱:** جدول نهایی را بساز: چیدمان روی هر بازهٔ N و هر دو جهت، به‌اضافهٔ میدان. میانهٔ ۳ تکرار، کمینه و بیشینه، و نسبت تازه به قدیم.
- [ ] **گام ۲:** آن را در `docs/architecture/wip/r1/report.fa.md` بنویس، ثبت بزن (`R1 گام 10: گزارش قدیم در برابر تازه`) و گزارش فارسی را به صاحب پروژه بده، با لینک جدول پیشرفت.

## گام ۱۱ روش: تصمیم پایان فاز

### کار ۲۵

- [ ] **گام ۱:** اگر همهٔ معیارهای §۵.۱ در همهٔ بازه‌های N و هر دو جهت سبزند و بهبود روشن است:
  - `main` روی گیت‌هاب fast-forward می‌شود (هرگز force-push).
  - ثبت بزن: `R1 گام 11: main به build …`.
  - R2 بلافاصله شروع می‌شود.
- [ ] **گام ۲:** وگرنه فاز باز می‌ماند و `main` جابه‌جا نمی‌شود:
  - علت با سنجش و پروفایل پیدا می‌شود.
  - نامزد تازه ساخته می‌شود و از گام ۳ روش دوباره اجرا می‌شود.
  - گزارش هر دور داده می‌شود.
  - این تا رسیدن به بهبود روشن ادامه دارد.
