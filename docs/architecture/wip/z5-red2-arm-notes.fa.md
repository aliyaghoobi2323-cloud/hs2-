# تیم قرمز دور ۲ — بازوی smux، استخر پنجره، سازگاری (Z5)
- درخت: scratchpad/z16/arm، شاخهٔ z5-red2-arm (پایه 451bb87). بی push.
- اجرا: taskset -c 3 nice -n 19 go test -p 1.
- لاگ‌ها: scratchpad/z16/logs/

## وضعیت
- [ ] خواندن یادداشت‌ها و کد (انجام شد: pool.go، core.go، تفاوت session/stream)
- [ ] اجرای آزمون‌های پایهٔ smux
- [x] آزمون‌های پایهٔ smux (سنجاق‌شده): فقط TestAdaptiveBrakeUpdatesInOrder شکست (از پیش شناخته).
- [x] ثبت 4ff6bbd: آزمون آشوب مرگ ناگهانی (red2_chaos_test.go) + HS2_RED2_SEEDS.
- [x] یافتهٔ ۱ (ثبت 257902b): یدک قرضی نشست بسته در Close فوراً به استخر برمی‌گشت در حالی که داده‌اش تا ۵s در حافظه بود. اصلاح اعمال شد. آزمون TestRed2BorrowedDataKeepsItsPoolAfterClose (پیش: «614400 past MRB, pool holds 0»).
- [ ] در جریان: آشوب ۲۴ بذر با race (لاگ logs/chaos-race-24.log، باینری پیش از اصلاح ۱).
- [x] «گیر» بذر ۱۲۰ در TestRedNoStallUnderSweepsAndChaos (اجرای ۲۴ بذر با race، پیش از اصلاح ۱): ساختگی آزمون — خوانندهٔ جریان i=7 بافر ۱ بایتی کشیده بود (1+r.Intn(64K))، ۳MiB یک بایت در هر Read زیر race چند دقیقه. رونوشت گوروتین‌ها: logs/stall-seed120.log؛ با dumpStreams: «stream 17 rcvWin 16384 ... unread 104675»، خواننده زنده. اصلاح آزمون: کف ۱KiB برای بافر. نقص smux نیست.
- [x] یافتهٔ ۲ (طراحی، کم): قرض زیر قاعدهٔ سهم نیست → یک لینک با اپ یخ‌زدهٔ تند + خواننده‌های ایستاده کل استخر را می‌گیرد. TestRed2OneLinkTakesThePoolByBorrowing: «pool 1536 KiB: the frozen link holds 1536 KiB (claims 768, borrowed 768); free 0 KiB. The other link: window up to 256 KiB (need 256), 512 KiB taken in of 576 sent, borrows 0, parks 2». پیشنهاد، اعمال نشد.
- [x] مسابقهٔ held کهنه در takeFor (دو جریان یک نشست هم‌زمان): نظری؛ با سنجاق تک‌هسته ۴۰ دور، extra هرگز از free نگذشت. گزارش نمی‌شود.
- [x] سند §۸.۳ برای قرض به‌روز شد (ثبت بعدی).
- [ ] آشوب ۲۴ بذر race پس از اصلاح ۱ (logs/chaos-race-24b.log) در جریان.
- [ ] آزمون موتور TestRed2OldExitRealStatsNoCeiling نوشته شد (engine/red2_wcore_test.go)، اجرا نشده.
- [ ] compat (z16/compat: پرچم -pool، -only multi|stall|die)، redcmp (z16/redcmp)، GOARCH=386.
- [x] آشوب ۲۴ بذر با race پس از اصلاح ۱ (logs/chaos-race-24b.log): همه گذشت (TestRed2ChaosMoreSeeds 161s، Abrupt ۲۴ بذر، و آزمون‌های دور ۱).
- [x] ثبت 7609e18 (آزمون یک‌لینک‌کل‌استخر، dumpStreams، کف بافر، §۸.۳).
- [x] smux کامل سنجاق (logs/smux-full.log): فقط TestAdaptiveBrakeUpdatesInOrder (از پیش). race (smux-race.log): بی DATA RACE؛ فقط TestAdaptiveNoDeadlockSmallMin، که در پایه هم با race و سنجاق ۳ از ۳ شکست. GOARCH=386 (smux-386.log): فقط BrakeUpdatesInOrder. -count=3 روی Pool/Fix/Red/Core: ok.
- [x] redcmp (z16/redcmp): ۱۲ اجرا در هر درخت؛ md5 غالب 2ec79876 (همان دور ۱): پایه ۱۱/۱۲، هسته ۱۰/۱۲؛ انحراف‌ها توالی ترمز (زمان‌بندی) در هر دو؛ بیشینهٔ پنجره در همه ۲۶۲۱۴۴ (Need).
- [x] compat (z16/compat، logs/compat-*.txt): استخر ۳۲: هسته از upstream/b765677 پنجرهٔ ۱۶MiB، ۸۰٫۷Mbit/s، تخلیه به ۳MiB رعایت، ۰٫۸۱ UPD/MiB (پایه ۱٫۰۳)، استخر کامل. استخر ۱۶: پنجره ≤ ۱۰MiB (نصف استخر)، ۷۴٫۷، ۰٫۹۷ UPD/MiB. سه جریان: استخر حداکثر ۱۶ از ۳۲ و ۸ از ۱۶ (سهم)، سالم. توقف ۳s خواننده: سالم، استخر کامل. مرگ لینک: ۱۴MiB پیش، ۸٫۷ درست پس از مرگ (= خوانده‌نشده − Need)، با Close اپ فوراً کامل، بی Close پس از ۱۰s.
- [ ] زیرمجموعهٔ موتور (logs/engine-subset.log) در جریان؛ سپس «بی اصلاح» برای TestRed2DeadLinkKeepsBorrowedPoolUntilRelaysLetGo.
- [x] زیرمجموعهٔ موتور (logs/engine-subset.log): همه گذشت، از جمله TestRed2OldExitRealStatsNoCeiling («78 records of 64 bytes, 15 MiB downloaded, 18 evaluations, core OFF throughout») و TestRed2DeadLinkKeepsBorrowedPoolUntilRelaysLetGo («borrowed 3072 KiB; ... whole 1s after it»). بی اصلاح ۱: «the link died holding 3072 KiB borrowed ...: the pool holds 0 KiB for it».
- [x] ثبت b5319eb (دو آزمون موتور).
- [x] vet روی ۳۸۶ (engine، tune، smux) و ساخت arm7: پاک.
- [ ] go test ./... سنجاق (logs/all.log) در جریان.
- [x] go test ./... با سنجاق (logs/all.log): engine ok (۵۶۳s)، smux جداگانه بالا. دو شکست بی‌ربط: cmd/hs2 TestCPUMeter (سنجش CPU زیر بار؛ تنها ۳ از ۳ گذشت) و tun TestOffloadDeviceUDP (فرمان ip نیست).
- وضعیت: **کار تمام است؛ گزارش نهایی پایین.**

## گزارش نهایی (HEAD b5319eb، شاخهٔ z5-red2-arm، بی push)
ثبت‌ها: 4ff6bbd (آشوب مرگ ناگهانی)، 257902b (اصلاح یافتهٔ ۱)، 7609e18 (آزمون یافتهٔ ۲، نمایش گیر، §۸٫۳)، b5319eb (دو آزمون سرتاسری موتور).

### یافته‌ها به ترتیب شدت
1. متوسط — قرض نشستِ بسته همان لحظهٔ Close به استخر برمی‌گشت، با داده‌اش هنوز در حافظه (اعمال شد، 257902b).
   - ریشه: session.go:281 (Close → dropSpare) همهٔ یدک را می‌داد؛ اصلاح دور ۱ (f1ff2d9) فقط ادعای پنجره‌ها را با داده نگه داشت. Stream.Close روی نشست بسته توکن‌ها را به سطل برنمی‌گرداند.
   - اندازه: تا ۲·MRB + MRB/8 = ۱۷MiB برای هر لینک در حال مرگ، جمعاً تا کل استخر (۱۶ تا ۱۲۸MiB، ×۱٫۵ گردکردن)، به مدت relayDieGrace (۵s). ادعای tune.go و §۸٫۳ («سهم استخر فراتر نمی‌رود») برای قرض نادرست بود.
   - اصلاح: یدک در Close فقط تا جایی که سطل آزاد است برمی‌گردد (returnSpare با room=0 پس از بسته شدن)؛ باقی با خواندن یا Close جریان (که حالا توکن‌ها را هم برمی‌گرداند) یا closeGrace (dropClaims حالا یدک را هم می‌دهد). letGo پس از بستن سطل را هم کم می‌کند تا رابطهٔ «سطل = MRB + extra − خوانده‌نشده» بماند.
   - شکست بی اصلاح: smux TestRed2BorrowedDataKeepsItsPoolAfterClose «link closed holding 1138688 unread (614400 past its MaxReceiveBuffer, all of it borrowed); the pool holds 0 for it» در هر سه حالت؛ engine TestRed2DeadLinkKeepsBorrowedPoolUntilRelaysLetGo «the link died holding 3072 KiB borrowed, its relays still holding the data: the pool holds 0 KiB for it». با اصلاح هر دو گذشتند.
2. کم (طراحی) — قرض زیر قاعدهٔ سهم نیست؛ یک لینک با اپ یخ‌زدهٔ تند و خواننده‌های ایستاده کل استخر را می‌گیرد (پیشنهاد، اعمال نشد).
   - TestRed2OneLinkTakesThePoolByBorrowing: «pool 1536 KiB: the frozen link holds 1536 KiB (claims 768, borrowed 768); free 0 KiB. The other link: window up to 256 KiB (need 256), 512 KiB taken in of 576 sent, borrows 0, parks 2».
   - ریشه: pool.go borrow از take استفاده می‌کند نه takeFor (عمدی، §۴٫۵). پسرفت نسبت به پایه نیست؛ نگهبان گیر پس از چند ثانیه آزادش می‌کند.
   - پیشنهاد: قرض با سهم روی یدک خودش (spare + k ≤ free − k): بی ادعا همان ۲·MRB روی استخر ۳۲MiB، با ادعای نصف استخر ~۸MiB. باید در چیدمان ۱Gbit/100ms (حالت TestPoolBorrowNextToAGrownWindow) سنجیده شود.
3. ساختگی آزمون (اصلاح آزمون، 7609e18) — «گیر» بذر ۱۲۰ در TestRedNoStallUnderSweepsAndChaos با race: بافر خواندن ۱ بایتی؛ نقص smux نیست.

### زاویه‌هایی که سالم ماند
- نشت/بازگشت دوباره: TestRed2AbruptDeathChaos (۲۴ بذر، race): سه شکل مرگ، دفتر سخت‌گیرانه برای نشست بسته هم، ناظر استخر؛ استخر هیچ‌گاه بالای اندازه یا زیر صفر نرفت و پس از closeGrace کامل بود.
- آشوب دور ۱ با ۲۴ بذر دیگر (۱۰۰ تا ۱۲۳) با race؛ ‎-count=3؛ ۳۸۶.
- قاعدهٔ سهم: compat سه‌جریانی: ≤ نصف استخر (۱۶ از ۳۲، ۸ از ۱۶)؛ takeFor هرگز قفل نمی‌کند؛ مسابقهٔ held کهنه دیده نشد.
- turnIdle: شکاف ≥ ۱s فقط وقتی W < نرخ×(چرخش − ۱s)، زیر BDP؛ صف فقط با W > BDP ساخته می‌شود که داده بی‌شکاف است و هر چرخش نمونه می‌شود.
- ceil=0: redcmp ۱۲+۱۲ اجرا، md5 غالب یکی (2ec79876)؛ بیشینهٔ پنجره = Need.
- خروجی قدیمی روی جفت واقعی: TestRed2OldExitRealStatsNoCeiling.
- compat با upstream v1.5.24 و b765677، هر دو سو، استخر ۳۲ و ۱۶، توقف خواننده، مرگ لینک: داده سالم، پنجره‌ها رعایت، استخر کامل.
- کران حافظه پس از اصلاح ۱: Σextra ≤ استخر برای نشست‌های زنده و مرده؛ تنها فرارفت مستند همان MRB لینک مرده (تا ۵s) است.
- پیش‌موجود: TestAdaptiveBrakeUpdatesInOrder (سنجاق)، TestAdaptiveNoDeadlockSmallMin (race + سنجاق؛ در پایه هم ۳ از ۳)، TestCPUMeter، TestOffloadDeviceUDP.
