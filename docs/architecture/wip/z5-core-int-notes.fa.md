# یادداشت‌های گام ۶ و ۷ (سیم‌کشی هستهٔ پنجره و آزمون کامل) — شاخهٔ z5-int

درخت کار: /tmp/claude-0/-home-user-hs2-/48e4aec7-ad72-5cc1-b09e-d3366fa010b6/scratchpad/z10/int
**کار تمام است؛ گزارش نهایی فرستاده شد.** (worktree موقت inthead برای مقایسه با f8dfe77 حذف شد.)
اگر از نو شروع شد: این فایل و `git log --oneline f8dfe77..` را بخوان و از نخستین کار ناتمام ادامه بده.

## وضعیت
- [x] ۳: هم‌ترازی Hits (بازو ← نشانهٔ فاصلهٔ مدل) — 6e43512
- [x] ۲: مدل به smux.StepOf / smux.FitCore — 6e43512 (خروجی جدول مدل بی‌تغییر)
- [x] ۱: wcore_run.go (رجیستری، اجراکننده، سقف‌ها) — 1ddcde3، be28c72
- [x] ۴: apProbing و grownRate — 1ddcde3، a7b0751
- [x] ۵: لاگ‌ها، یادداشت پایان لینک، شمارنده‌ها، سطر وضعیت — 1ddcde3، a7b0751
- [x] ۶: دستگیره‌ها — 1ddcde3، 2b00a56
- [x] ۷: آزمون ناپایدار چکه‌ای — cdc9ddc
- [x] آزمون‌های اجراکننده و جهش‌ها
- [x] گام ۷: آزمون کامل (نتیجه در بخش «آزمون‌ها»)

## ثبت‌ها روی z5-int (پس از f8dfe77)
| ثبت | محتوا |
|---|---|
| 6e43512 | hit بازوی smux = مکث در حد پیشین (hitGap=2ms)؛ CoreSample.Stream؛ مدل و wcStep روی smux.StepOf/FitCore |
| 1ddcde3 | wcore_run.go، سیم‌کشی موتور، apProbing، grownRate، لاگ‌ها، یادداشت‌ها، دستگیره‌ها، آزمون‌ها |
| cdc9ddc | رفع ناپایداری TestWedgeGuardTricklingReaderDoesNotHideStall (فقط آزمون) |
| be28c72 | رفع سرریز بی‌علامت در خواندن پونگ‌ها؛ آزمون ورودی‌های لبه و مرجع نشست |
| a7b0751 | سطر «windows:» در hs2 status؛ اوج نمایشی gShown |
| 2b00a56 | آزمون دستگیره‌ها |
| de59ec9 | همان مسابقهٔ آزمون در TestWedgeGuardReleasesStuckReaders (ابزار مشترک openHeld)؛ BUILD.md دستگیره‌ها |
| 5838ca9 | سطر «fill the path» بودجهٔ نگه‌داشته را می‌گوید (کف n·c_min)، نه زانوی زیر کف |

## اجراکننده در چند سطر
- `newSession` پس از `guards.add` با `wcores.add(sess, cfg, gate.tcp, meter, server, remote)` نشست را ثبت می‌کند، فقط اگر `Wcore != 0` و پیکربندی پنجرهٔ تطبیقی، `NeedStreamBuffer < MaxStreamBuffer` و `WindowPool` دارد. لبه (smux client) = دانلود، خروجی (server) = آپلود. `meter.wc` نگه داشته می‌شود.
- یک goroutine پردازه‌ای هر ۱۰۰ms (`wcRampTick`) بیدار می‌شود؛ نشست بسته از رجیستری حذف می‌شود؛ لینکی که `next`ش نرسیده رد می‌شود؛ لینکی که هسته‌اش OFF است و از آخرین عکس کمتر از ۱۲۸KiB خوانده، با یک load اتمی رد می‌شود (۴۰۰ms بعد دوباره).
- هر گام: یک `TCP_INFO` (min_rtt، rtt، segs_in و rcv_ooopack با باز کردن wrap ۳۲ بیتی)، یک `CoreSnapshot` (تنها فراخواننده)، `WindowPool.Free()`؛ روی لبه `rttMin`، پونگ‌های تازه از حلقهٔ ۴تایی `linkMeter.pongRTT`، آخرین رکورد خروجی (`srtt==0` یعنی نیست) و `apProbing` مدیر لینک. `wcBuilder.build` → `wcore.step` → (اگر FIXEDWIN: `pin`) → `apply`: اول `SetCoreRef`، بعد سقف هر جریان با شناسه (اشاره‌گر `CoreSample.Stream`)، و `SetCoreCeil(0)` برای جریانی که از عکس بیرون رفت → `publish` (اتمی‌ها) → ردگیری و لاگ.
- بُرش میان ارزیابی‌ها (`wcore.cut`) رویدادی نمی‌سازد؛ اجراکننده آن را از بالا رفتن `cnt.drainLoss` بی رویداد DRAIN می‌شناسد و یک رویداد DRAIN (اتلاف، q<0) برای لاگ و ردگیری می‌سازد.

## دستگیره‌ها (applyTuning)
| دستگیره | متغیر | معنا |
|---|---|---|
| `HS2_TUNE_WCORE=0` | `engine.Wcore` | هسته خاموش؛ هیچ نشستی ثبت نمی‌شود (کلید ایمنی میدانی) |
| `HS2_TUNE_WCORE_QTARGET=<ms>` | `engine.WcoreQTarget` | qTarget هر دو سر (wcQTarget و wcQTargetExit) |
| `HS2_TUNE_WCORE_PROBEQ=<ms>` | `engine.WcoreProbeQ` | probeQ هر دو سر |
| `HS2_TUNE_WCORE_STAGE=2` | `engine.WcoreStage` | گام ۲ (سقف زیر Need پس از پر شدن مسیر) |
| `HS2_TUNE_WCORE_TRACE=<path>` | `engine.WcoreTrace` | یک سطر JSON برای هر ارزیابی (و هر بُرش) هر لینک |
| `HS2_TUNE_WCORE_FIXEDWIN=<bytes>` | `engine.WcoreFixedWin` | فقط آزمایشگاه: سقف همهٔ جریان‌های F برابر این پنجره، بی‌ترمز رشد (turnRef=0، growOK=true)، ردگیری ادامه دارد و هیچ لاگ تصمیمی نوشته نمی‌شود |

QTARGET و PROBEQ و TRACE هنگام ثبت نخستین نشست خوانده می‌شوند؛ بقیه هنگام ثبت هر نشست.

## سطرهای لاگ (عین متن)
- `mtcp: link 3: downloads fill the path at ~287 Mbit/s (round trip 151 ms): window held at 6.4 MiB for 2 fast connection(s), queue ~4 ms` — پایان STARTUP، یا بودجهٔ HOLD بیش از ۲۵٪ دور از سطر قبلی؛ هر لینک ۱ در ۶۰s.
- DRAIN (هر لینک ۱ در ۳۰s): `mtcp: link 3: <علت> — download windows 6.2 MiB → 3.0 MiB` با علت یکی از:
  `path queue grew to 38 ms` / `path queue grew to 38 ms (resent 2.1%)` / `packets were lost as the windows grew (resent 2.1%)` / `packets arrived behind holes as the windows grew` / `packets arrived behind holes on a clean path` (بُرش) / `path slowed to ~50 Mbit/s with its queue growing`.
- `mtcp: link 3: 0.4% loss with no queue — windows cover the resends (+18 ms)` — ۱ در ۱۰ دقیقه.
- `mtcp: link 3: one connection's window is at its 16 MiB ceiling (round trip 480 ms)` (یا `N connections' windows are at their 16 MiB ceiling`) — ۱ در ۱۰ دقیقه؛ اگر سقف از استخر بود، به سطر استخر می‌رود.
- `mtcp: window pool full (128 MiB): 5 fast connection(s) on 3 link(s) held below what their path carries` — ۱ در ۵ دقیقه برای پردازه، لینک‌های ۱۰ ثانیهٔ اخیر.
- `mtcp: link 3: the path's round trip grew to ~180 ms (a route change) — the window core follows it` — ۱ در ۱۰ دقیقه.
- جمعی (بالای ۸ لینک در دقیقه): `mtcp: window core: 4 more link(s) found the window that fills their path in the last minute (6 fast connection(s), ~900 Mbit/s)` و `mtcp: window core: 4 more link(s) drained their windows 4 time(s) in the last minute (a queue grew, or loss came, on their path)`.
- خطای فایل ردگیری: `mtcp: window core trace: <err>`.
- سمت خروجی: `link from 1.2.3.4:5678`، «uploads»/«upload windows».
- یادداشت پایان لینک (فقط اگر هسته از OFF بیرون آمده): لبه در `linkStallNote`: ` (while up: …; window core: up to 9.1 MiB at 412 Mbit/s, drained 3 time(s) for queue, 1 for loss[, 2 for a slower path], held by the window pool 0 time(s))`؛ خروجی مستقیم: دنبالهٔ `link down from …`؛ خروجی معکوس: پس از دلیل در `serveReverseLink` و جدا شده با `cutEndNote` در `exit_pool` تا `edgeClosed` درست بماند.
- وضعیت: `  windows:    fast connections: 3 (windows 14 MiB of 128 MiB pool)`.

## تغییر در کنترل‌گر یا مدل
- منطق کنترل‌گر دست نخورد. فقط `wcStep` حالا خود `smux.StepOf` را صدا می‌زند (حالت need=0 حذف شد؛ اجراکننده بی Need ثبت نمی‌کند) و توضیح سرآغاز به‌روز شد.
- مدل: نسخه‌های محلی `modelStepOf`/`modelFitCore` حذف و به smux وصل شد؛ خروجی جدول پیش‌فرض بایت‌به‌بایت همان ماند.

## هم‌ترازی Hits
- بازو: hit یعنی «پس‌ماندهٔ گیرنده به حد آخرین UPD رسید» (`recv+frame ≥ limNow`). برای خوانندهٔ هم‌پا این تقریباً هرگز رخ نمی‌دهد (باید unread به نیم‌پنجره برسد)؛ یعنی hit فقط خوانندهٔ عقب‌مانده را نشان می‌داد، نه فرستندهٔ محدود به پنجره.
- مدل: «فاصله در حد پیشین»: دادهٔ رسیده به یک قاب مانده تا `L_prev` یک UPD در حلقه، و گذشتن از آن دست‌کم ۲ms بعد.
- تصمیم: بازو به تعریف مدل برگشت (کنترل‌گر با آن واسنجی شده). `turnMark.reach`، `hitGap = 2ms`، `limNow` حذف. آزمون‌ها: `TestCoreHitIsAPauseAtTheLimit` (قطعی) و `TestCoreHitsAndLags` با همتای یک UPD عقب.
- محدودیت (برای سنجش): زیر حدود ۶۵Mbit/s برای هر جریان فاصلهٔ خود قاب‌های ۱۶KiB از ۲ms بیشتر است؛ مدل همین را دارد.

## آزمون‌ها
همه با `nice -n 19` و `-p 1`. دو اجرای نخست هم‌زمان با سنجش‌های TestZ5Rig بود؛ بقیه پس از پایان آن‌ها.
- `go vet ./...` (hs2-src): تمیز. smux: فقط هشدار قدیمی `session_test.go:301`. `GOOS=linux GOARCH=386 go vet ./engine/` تمیز.
- `GOOS=linux GOARCH=386 go build ./...` و `GOARCH=arm GOARM=7 go build ./...`: گذشت.
- smux (`cd hs2-src/third_party/smux`): `go test .` گذشت (۲۰s)؛ `-race` گذشت (۴۰s)؛ `GOARCH=386 -run 'TestCore|TestPool'` گذشت.
- `go test ./...` اجرای ۱ (2b00a56، زیر بار سنجش): شکست‌ها فقط `tun/TestOffloadDeviceUDP` (ابزار ip نیست؛ شناخته)، `TestWedgeGuardReleasesStuckReaders` («4 از 5») و `TestFastLinkWithoutCeilingTakesNoPool` («read 0, timeout»). هر دو پیش از این کار هم هستند (پایین).
- `go test -v ./engine/` اجرای ۲: همه گذشت (۴۰۷ گذشت، ۱ رد، ۰ شکست؛ ۵۰۹s).
- `go test -race ./engine/` روی 5838ca9: گذشت (۵۸۲s).
- `go test ./...` نهایی روی 5838ca9 (بی سنجش هم‌زمان): همه گذشت به‌جز `tun/TestOffloadDeviceUDP` (ابزار ip نیست؛ شناخته). موتور ۵۱۰s.
- `HS2_WCORE_MODEL_FULL=1 -run TestWcoreModel`: گذشت؛ «30 of 85 cells meet every target»؛ خروجی با f8dfe77 بایت‌به‌بایت یکی (پس از حذف شمارهٔ خط).
- پس‌نزدن (§۹.۵)، بی تغییر انتظار: loss_test ۹/۹، quality_test ۹/۹، rescue_test ۱۳/۱۳، rescue_late ۱/۱، rescue_quiet ۵/۵، rescue_stuck ۳/۳، sendgate ۵/۵، wedge ۶/۶ (از جمله TestSlowReadersDoNotStallLink و TestReadersSlowingDownDoNotStallLink)، windowpool ۳/۳؛ smux: stream_adaptive، stream_brake، shaper همه گذشت.
- آزمون چکه‌ای: پیش از اصلاح ۸ از ۱۰ (`-count=10`، زیر بار)، پس از اصلاح ۱۰ از ۱۰ (رهاسازی در ۷ تا ۹ ثانیه، حد ۱۲)؛ با `wedged := false` شکست («a stalled relay still holds the link after 20s»)؛ با بازگرداندن قاعدهٔ قدیم «صفر خواندن» (`calls == g.lastCalls`) شکست («released only after 16s»).
- TestWedgeGuardReleasesStuckReaders: همان مسابقه (۵ یخ‌زده × 256KiB > سطل 1MiB). پایه f8dfe77: ۱ از ۱۸۵؛ شاخه پیش از اصلاح ۳ از ۸۵؛ شاخه با `Wcore = 0` ۲ از ۱۰۰ (پس اجراکننده علتش نیست). پس از openHeld: ۱۰۰ از ۱۰۰؛ با نگهبان غیرفعال «guard closed 0 stuck relays, want 5».
- جهش: ۱۸ جهش روی کد تازه (پاک نکردن سقف جریان رفته، pin بی‌اثر، نخواندن apProbing، noteCap و gPeak با grown، بی سقف تکرار، بی جمع‌بندی، ماندن نشست بسته، منتشر نکردن apProbing، نادیده گرفتن WCORE=0، grown در sampleHealth، یادداشت پایان لینک، بی ردگیری، بی SetCoreRef، ننوشتن حلقهٔ پونگ، نخواندن رکورد خروجی، نادیده گرفتن سقف‌ها در apply، grownRate صفر) همه کشته شدند؛ ۲ جهش hit در smux («همیشه» و «هرگز») هم.
- زمان آزمون‌های تازه: سرتاسری رشد و تخلیه حدود ۹s، پنجرهٔ ثابت حدود ۴s، خاموش ۳s، ورودی‌های لبه ۳s (پینگ نخست)، بقیه زیر ۲s.

## سطرها (f8dfe77..HEAD)
- کد: +1087 −75 (wcore_run.go ۹۱۸ سطر، بقیه سیم‌کشی کوچک در ۱۲ فایل)؛ آزمون: +1282 −97 (wcore_run_test.go ۱۰۸۶)؛ مستند: +9 −1 (BUILD.md).

## یافتهٔ جدی پیش‌موجود (بیرون از این کار)
`TestFastLinkWithoutCeilingTakesNoPool` گاهی داده را **گم می‌کند یا می‌ایستد**: در ۱۵۰ تکرار روی شاخه ۳ شکست («read 0, timeout» دو بار، «read 100597760, EOF» یعنی ۶۴KiB کم با EOF). روی f8dfe77 هم ۵ از ۱۵۰، و همان آزمون (بی بررسی استخر) روی 4bc9cd1، پیش از بازوی smux و هر چیز Z5، ۸ از ۱۵۰ («read 0..131072, timeout» و کمبود ۱۶ تا ۴۸KiB با EOF). پس در پشتهٔ موجود (smux یا shaped/sendGate/meteredConn روی loopback واقعی) است و ربطی به هسته ندارد؛ ولی کمبود داده با EOF و ایستادن کامل ۶۰ ثانیه‌ای باید جدا بررسی شود (بازتولید: `go test -count=150 -run TestFastLinkWithoutCeilingTakesNoPool ./engine/`، زیر بار).

## مسئله‌های باز برای گام سنجش
1. **q_ref با صف ورود به HOLD:** در آزمون سرتاسری، کند شدن مسیر درست هم‌زمان با ورود به HOLD بود؛ نخستین دورهٔ HOLD `q_ref = 345ms` گرفت و DRAIN فقط چون صف باز هم بالا رفت آمد (۴۳۱ > ۳۴۵ + ۳Δq). اگر صف در همان سطح می‌ماند، هسته صف ایستا را «مرجع» می‌گرفت و `rLinkHold` هم از همان نرخ کند شروع می‌شد. منطق کنترل‌گر است (refPending)؛ دست نخورد.
2. **نشانهٔ hit زیر حدود ۶۵Mbit/s هر جریان:** فاصلهٔ خود قاب‌های ۱۶KiB از ۲ms بیشتر است، پس گذر از L_prev در جریان کند هم hit شمرده می‌شود (مدل هم همین را دارد). در ردگیری سنجیده شود؛ شاید آستانه نسبی به فاصلهٔ معمول قاب‌ها لازم باشد.
3. **T_base بی TCP_INFO:** اجراکننده `min_rtt` را فقط از سوکت لینک دارد؛ روی لبه پشتیبانش `rttMin` کنترل است و روی خروجی هیچ (هسته OFF می‌ماند). در چیدمان TUN واقعی min_rtt هست.
4. **سمت خروجی (آپلود):** نه q_snd، نه apProbing؛ در مدل سنجیده نشده؛ ردگیری آن در سنجش لازم است.
5. **FIXEDWIN:** ماشین حالت هسته در پس‌زمینه می‌چرخد؛ در ردگیری state و B را باید نادیده گرفت (فقط سیگنال‌ها و `fixed`). F از قاعدهٔ خود هسته می‌آید؛ پنجرهٔ ثابت زیر need/2 فقط برای جریانی است که پیش‌تر در F بوده.
6. **ردگیری:** یک سطر برای هر ارزیابی و نیز برای هر بُرش میان ارزیابی‌ها (رویداد DRAIN ساخته‌شده). فایل یک بار در نخستین ثبت باز می‌شود؛ مسیر بعدی اثری ندارد.
7. **هزینهٔ پردازنده:** هر لینک فعال هر ۱۰۰ms یک TCP_INFO و یک CoreSnapshot (قفل streamLock برای کپی فهرست)؛ لینک OFF بیکار فقط یک load اتمی. با صدها لینک فعال در cpuMB سنجیده شود.
8. **grownRate** با `turnBase` (کمینهٔ چرخش ۶۰ ثانیه) حساب می‌شود، پس محافظه‌کار است (کمترین grown). اوج نمایشی `peak` در وضعیت با کل ترافیک (gShown) ماند؛ اوج تصمیم و ظرفیت هر لینک بی grown.
9. **سطر وضعیت:** «windows» جمع کل پنجره‌های جریان‌های بالای Need است، نه فقط سهم استخر.
10. **apProbing** برای هر LinkManager است و فقط لبه آن را دارد (طرح §۱۰ خطر ۱۱).
11. **سطر «fill the path» برای جابه‌جایی زانو:** فقط در HOLD و با فاصلهٔ بیش از ۲۵٪ از آخرین سطر؛ پس از DRAIN مرجع جابه‌جا نمی‌شود.
12. **پیش‌فرض روشن:** همهٔ نشست‌های آزمون‌های موجود موتور حالا هسته دارند (مثل تولید)؛ اجرای کامل و race سبز است.
