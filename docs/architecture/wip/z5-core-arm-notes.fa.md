# گزارش بازوی smux و سیگنال‌های موتور (گام‌های ۱ تا ۳ طرح WINDOW_CORE)

درخت کار: `/tmp/claude-0/-home-user-hs2-/48e4aec7-ad72-5cc1-b09e-d3366fa010b6/scratchpad/z10/arm` (شاخهٔ `z5-arm`، پایه `4bc9cd1`). چیزی push نشده است.
اگر از نو شروع شد: این فایل و `git log --oneline 4bc9cd1..` را بخوان. **کار تمام است.**

## ثبت‌ها
| ثبت | محتوا |
|---|---|
| `23c0851` | گام ۱: استخر پنجره، ادعا، `reclaim`، `claimHold`، `sweepClaims`، `borrow`، `Extra`/`Borrows`، حساب `tune.go`، `minRTT`، پیکربندی موتور، دستگیره‌های `NEEDBUF` و `POOL` |
| `654e0e0` | گام ۲: بازوی سریع (`core.go`): سقف، `FitCore`، `StepOf`، نمونه‌گیر چرخش، شمارنده‌ها، `CoreSnapshot` |
| `7353600` | گام ۳: `rcvOOO` و `bytesReceived` در `tcpStat`، رکورد ۹۶ بایتی `kindStats` |
| `a0e8178` | مستندات کوتاه README و BUILD.md (سقف لینک با استخر، دستگیره‌ها) |
| `bb11db7` | دقیق‌تر کردن توضیح سرآغاز `core.go` |

## API صادرشده (third_party/smux)
```go
// پیکربندی
Config.NeedStreamBuffer int   // سقف پنجره بی‌هسته (موتور: 2MiB)؛ 0 = مثل قبل تا MaxStreamBuffer
Config.WindowPool *WindowPool // استخر پردازه (به NeedStreamBuffer نیاز دارد)
func NewWindowPool(size int) *WindowPool
func (p *WindowPool) Free() int

// توابع خالص (قرارداد با عامل دیگر)
func StepOf(need, w uint32) uint32
func FitCore(old, want, ceil, need, minWin, unread uint32, growOK, turnOK bool) uint32

// هسته ← جریان/نشست (بی‌قفل)
func (s *Stream) SetCoreCeil(ceil uint32) // تا MaxStreamBuffer بریده می‌شود؛ 0 = بی‌سقف (≤ Need)
func (s *Stream) CoreCeil() uint32
func (s *Session) SetCoreRef(turnRef, qBrake time.Duration, growOK bool)

// جریان/نشست ← هسته
type CoreSample struct {
	ID, Ceil, Win     uint32        // Win = winNow (پنجرهٔ آخرین UPD)
	Read              uint64        // readTot
	Upds, Hits, Lags  uint32        // updN، hitN، lagN (تجمعی)
	TurnMin, TurnLast time.Duration // از عکس قبلی (با Swap صفر می‌شوند)
	PoolShort, Brakes uint32        // تجمعی، برای همین جریان
	Braked            bool          // ترمزی در ۱ ثانیهٔ اخیر (brakeDrain)
}
type CoreCounters struct {
	Parks, Brakes, Borrows, PoolShort uint32
	Extra int
	Rcvd  uint64 // کل بایت دادهٔ دریافتی نشست
}
func (s *Session) CoreSnapshot(dst []CoreSample) ([]CoreSample, CoreCounters)
func (s *Session) Extra() int
func (s *Session) Borrows() uint32
```
- `CoreSnapshot` فقط جریان‌هایی را می‌دهد که `Win ≥ need/4` یا از عکس قبلی hit تازه دارند؛ turnMin/turnLast **همهٔ** جریان‌ها را صفر می‌کند؛ `streamLock` فقط برای کپی فهرست گرفته می‌شود و `bufferLock` هیچ جریانی گرفته نمی‌شود. یک فراخواننده برای هر نشست.
- موتور: `SmuxStreamBuffer=16MiB` (سقف مطلق)، `SmuxNeedStreamBuffer=2MiB`، `SmuxWindowPool` (در `runCmd` از `tune.WindowPoolMiB`)، `windowPool()`؛ `tcpStat.minRTT/rcvOOO/bytesReceived`؛ `statsRec.srtt/minRTT/rttVar/retrans/segsOut`؛ `statsRecLen=96`، `statsRecMin=64`.
- tune: `WindowPoolMiB(ramMB)`، `PoolWorstCaseMiB(ramMB)`؛ `LinkWorstCaseMiB` و `LinkRAMPerLinkMB` حالا متغیرند (از `engine.SmuxSessionBuffer`).
- دستگیره‌ها: `HS2_TUNE_SMUX_NEEDBUF`، `HS2_TUNE_SMUX_POOL`.

## انحراف‌ها و تصمیم‌ها
1. **FitCore:** «رشد فقط با growOK و turnOK، حداکثر ×۲» فقط برای رشد **بالای need** است؛ رشد تا need مال خود خواننده است (شرط گام ۱: زیر ۲MiB هیچ فرقی با پایه نیست؛ مثلاً جهش نخست 256K→2M). کف تخلیه `max(minWin, unread)` هرگز از `want` بالاتر نمی‌رود؛ پس FitCore هیچ‌وقت بیش از want نمی‌دهد. ×۲ در uint64 حساب می‌شود (بی سرریز). نسخهٔ مدل عامل دیگر باید همین را داشته باشد.
2. **turnOK** با آخرین نمونهٔ داخلی چرخش (`turnSeen`، که هسته صفرش نمی‌کند) سنجیده می‌شود، نه `turnLast` که هسته Swap می‌کند؛ پیش از نخستین نمونه یا با `turnRef == 0` رشد آزاد است.
3. **claimHold:** `Config.RoundTrip` و `linkRTT` موتور حذف شدند. نگه‌داشت ادعا = `2 × max(turnSeen, turnRef)`، و اگر هیچ‌کدام نبود ۵۰۰ms.
4. **حلقهٔ چرخش:** حدها با `readTot` (۶۴ بیتی) حساب می‌شوند. `L_prev` بالاترین حد اعلام‌شده تا پیش از آن UPD است و فقط UPDی که آن را بالا می‌برد علامت می‌گذارد (UPD ترمز یا تخلیه نه). وقتی ۴ خانه پر است، تازه‌ترها نمونه نمی‌شوند (اگر قدیمی‌ترها رانده می‌شدند، با پنجرهٔ ۱۶MiB و گام W/8 هیچ نمونه‌ای نمی‌رسید).
5. **شمارنده‌ها:** `updN` در هر UPD (از جمله ترمز و sweep) زیاد می‌شود؛ `hitN` و `lagN` در همان لحظه برای چرخه‌ای که تمام شد. hit یعنی `دریافتی + frameSize ≥ حد UPD آخر`؛ lag یعنی `minUnread > StreamLagTarget/4` در چرخه.
6. **borrow** در N4 در `recvLoop` هر بار که سطل خالی است و `WindowPool != nil` فعال بود، **مستقل از پنجره‌ها**؛ دقیقاً همین ماند (با ceil=0 هم قرض می‌گیرد). مثل N4 هر borrow یک park هم شمرده می‌شود.
7. دستگیرهٔ `HS2_TUNE_SMUX_NEEDBUF` هم آمد، چون `HS2_TUNE_SMUX_STREAMBUF` حالا سقف مطلق (۱۶MiB) است و بی آن راهی برای تغییر پنجرهٔ پایه نبود.
8. **رکورد آمار:** پرچم «فیلد افزوده هست» اضافه نشد؛ صفر یعنی نیست (خروجی قدیمی یا بی TCP_INFO). این برای این بود که `TestStatsRecordWireLayout` بی‌تغییر بماند. لبه هر `recLen ≥ 64` را می‌پذیرد (`statsRecMin`).
9. **عددهای tune:** طبق §۸.۳ سقف خودکار عوض شد (۸GB/۸ هسته ۱۷۰←۱۶۲، ۴GB/۲ هسته ۸۵←۸۱، ۱۴٫۴GB/۴ هسته ۳۰۰←۲۸۶)؛ پس انتظارهای `tune_test.go` و `cmd/hs2/linkpool_test.go` (۱۷۰←۱۶۲ و متن دلیل) به‌روز شدند. این‌ها در فهرست پس‌نزدن نبودند و تغییرشان را خود طرح خواسته است.
10. `TestFrozenAppsFillTheGrownBufferThenTheGuard` (از N4): سمت خروجی هر کاربر یخ‌زده در goroutine جدا پذیرفته و خوانده می‌شود (`openFrozen`)، پس گوروتین آزمون دیگر روی Read خروجی‌ای که UPDش از لینک گیرکرده نمی‌گذرد نمی‌ماند؛ ۴۰ نگاه به‌جای ۲۰.
11. آزمون‌های قرض با `sendWithin` نوشته شدند تا اگر borrow برداشته شود **شکست بخورند، نه گیر کنند**.

## نتیجهٔ آزمون‌ها
- `cd hs2-src/third_party/smux && go test -p 1 .` ← گذشت (ماژول جداست؛ از `hs2-src` با `./third_party/smux/` اجرا نمی‌شود).
- `go test -race .` روی smux ← دو بار گذشت. `GOARCH=386 go test -run 'TestCore|TestPool' .` ← گذشت.
- `go test -p 1 ./engine/ ./tune/ ./cmd/...` ← همه گذشت (موتور ۴۹۰s).
- `go test -race ./engine/` کامل ← گذشت (۵۳۶s). سنجش‌ها (`TestZ5Rig/TestZ5LM`) هرگز بیکار نشدند، پس این اجرا در پایان، با `nice -n 19 -p 1` و هم‌زمان با آن‌ها انجام شد.
- `go vet ./engine/ ./tune/ ./cmd/...` تمیز؛ `go vet` در smux فقط هشدار قدیمی `session_test.go:301` را دارد که در پایه هم هست.
- `GOOS=linux GOARCH=386 go build ./...` و `GOARCH=arm GOARM=7 go build ./...` ← گذشت.
- پس‌نزدن: همهٔ آزمون‌های نام‌برده (`stream_adaptive_test`، `stream_brake_test`، `shaper_test`، `TestSlowReadersDoNotStallLink`، `TestReadersSlowingDownDoNotStallLink`، `rescue*`، `quality`، `sendgate`، قاعدهٔ اتلاف) بی تغییر انتظارها گذشتند.
- `TestFrozenApps…` زیر بار سنجش‌ها: ۵ از ۵، و با `-cpu 1` سه از سه.
- جهش: ۱۳ جهش کلیدی (حذف سقف بی‌هسته، نادیده گرفتن turnOK، گام نیم‌پنجره، حذف تخلیه، راندن قدیمی‌ترها در حلقه، حذف کف unread، حذف شمارش hit و lag، حذف آزادسازی ادعا هنگام بستن نشست، حذف borrow، نمونه از هر داده، نادیده گرفتن حد بالا، restore بی‌سقف، کف ۹۶ بایتی در لبه، پر نکردن فیلدهای افزوده) همه دست‌کم یک آزمون را شکستند.
- **ناپایدار زیر بار:** `TestWedgeGuardTricklingReaderDoesNotHideStall` روی پایه هم شکست می‌خورد. آزمون تشخیصی نشان داد: وقتی یکی از ۶ کاربر یخ‌زده پیش از پر شدن سطل هنوز داده نگرفته، «در حال نوشتن» نیست، نگهبان ۴ یا ۵ تای دیگر را می‌بندد، لینک باز جریان می‌یابد و آن یکی دیگر هرگز گیر نمی‌اندازد. این مسابقه در خود آزمون است. فراوانی شکست با بار عوض می‌شود: پایه ۱۹ از ۲۵ و شاخه ۹ از ۲۵ گذشت، ولی در دستهٔ آخر پایه ۰ از ۴ و شاخه ۱ از ۴. با بافرهای کوچک این آزمون، کد شاخه بی‌استخر منطقاً با پایه یکی است. در اجرای کامل موتور و در اجرای `-race` گذشت.
- `TestAdaptiveBrakeUpdatesInOrder` یک بار زیر بار شکست خورد (never braked)؛ ۵ تکرار بعدی و همهٔ اجراهای کامل گذشتند.

## سطرها
- کد: +۱۰۰۹ −۸۵ (گام ۱: +۵۳۳ −۳۹؛ گام ۲: +۴۱۰ −۲۵؛ گام ۳: +۶۰ −۱۷؛ بازچینی توضیح: +۶ −۴).
- آزمون: +۱۷۹۵ −۱۸ (گام ۱: +۴۳۰؛ گام ۲: +۱۱۱۶؛ گام ۳: +۲۴۹).
- مستندات: +۱۵ −۶ (README و BUILD.md).

## مسئله‌های باز برای یکپارچه‌سازی (گام ۶)
1. اجراکننده باید برای هر نشست فقط یک فراخوانندهٔ `CoreSnapshot` داشته باشد (turnMin/turnLast با هر عکس صفر می‌شوند).
2. هر borrow یک park هم هست. اگر هسته `growOK=false` را به `Δparks > 0` ببندد، borrow هم رشد را می‌بندد؛ برای جدا کردن از `Δ(Parks − Borrows)` استفاده شود.
3. `sweepClaims` پس از ۵s بی‌خوانی پنجره را حتی با سقف بالا به Need برمی‌گرداند (با یک UPD)؛ خواندن بعدی با ترمز رشد دوباره بالا می‌رود.
4. نسخهٔ مدل `FitCore` و `StepOf` باید با انحراف ۱ یکی شود.
5. با پنجرهٔ ۱۶MiB (گام W/8) حدود نیمی از UPDها نمونه می‌شوند (حلقهٔ ۴تایی).
6. چرخش برای فرستندهٔ کندمنبع بزرگ است؛ عضویت در F باید آن را بیرون بگذارد (طرح §۳.۵).
7. در رکورد آمار، `srtt == 0` یعنی `q_snd` در دسترس نیست (خروجی قدیمی یا بی TCP_INFO).
8. پیکربندی smux خروجی هم استخر و borrow دارد (`newSmuxConfig` مشترک است)؛ عمدی است.
9. یادداشت پایان لینک خروجی (`linkStallNote` سمت خروجی) و ادامهٔ «window core» در آن مال گام ۶ است.
10. CHANGELOG، ARCHITECTURE، ROADMAP و VALIDATION در گام ۱۰.
11. پیشنهاد جدا: آزمون خوانندهٔ چکه‌ای پیش از شروع ساعت صبر کند تا هر ۶ کاربر یخ‌زده داده گرفته باشند (این‌جا دست نخورد).
12. نسخهٔ پایهٔ مقایسه: `/tmp/claude-0/-home-user-hs2-/48e4aec7-ad72-5cc1-b09e-d3366fa010b6/scratchpad/z10/armbase/hs2-src` (از `git archive 4bc9cd1`).
