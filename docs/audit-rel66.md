# ممیزی نهایی کد — rel66 (head a9f6dd74)

- مبنای مقایسه: `origin/main` = rel54c (`5aebf590`، آخرین شاخه/برچسب منتشرشده در GitHub؛ `v-rel54c`). فاصله: 260 commit، حدود 9300 خط کد غیرآزمونی Go.
- روش: فقط خواندن کد روی نسخهٔ محلی (بدون آزمایشگاه، بدون سرور، بدون اجرای آزمون). چهار بخش موازی (smux، rescue/mconn، stripe، LB/مدیریت لینک) به‌علاوهٔ بررسی دستی envdef و مستندات. یافته‌های High با خواندن مستقیم کد دوباره تأیید شدند.
- شدت: **High** = قطع/ریست اتصال کاربر یا کوتاه‌شدن بی‌صدای داده در مسیر پیش‌فرض؛ **Med** = همان در شرایط باریک‌تر یا با اطمینان کمتر؛ **Low** = کارایی/شمارنده/تشخیصی یا فقط با کلید خاموش.

## ۱. یافته‌ها به ترتیب شدت

### High

**H1 — پیام RESCUE/ACK/NAK تکراری یا دیررس اتصالی را که نجات یافته ریست می‌کند** (اطمینان: بالا، کد تأیید شد)
- `engine/mconn.go:1506-1535` (هر handler که false برگرداند → `m.end(errMConnProto, true)`)
- `engine/mconn_rescue.go:705-713` (`onRescue` در خارج: اگر `rscPhase != 0` رد می‌کند)، `:761` (`onRescueAck`: `s != m.rscSub` → false)، `:792` (`onRescueNak` همان)، `engine/mconn_rescue_rec.go:471` (`onRRescueAns` همان الگو)
- سناریو ۱ (RETRY پیش‌فرض روشن روی ایران، rel64): RESCUE روی join1 به خارج می‌رسد و خدمت داده می‌شود (`rscPhase=2`)، ولی ACK در جهت معیوب گم می‌شود یا join1 در مجموعهٔ silent قرار می‌گیرد → `rescueRetrySilentLocked` روی join2 دوباره RESCUE می‌فرستد → خارج آن را refuse می‌کند → **ریست اتصال**. داده‌های `[Ri, Bk)` که خارج روی join1 فرستاد هم هرگز تکرار نمی‌شود.
- سناریو ۲: join1 فقط کند بوده (نه مرده)؛ بعد از retry، ACK اصلی روی join1 می‌رسد. readLoop join1 هنوز زنده است و `s != rscSub` → ریست در ایران. records-rescue (`rrRetryLocked`) همین مشکل را دارد.
- اصلاح پیشنهادی: پیام rescue تکراری/کهنه را no-op با `true` در نظر بگیرید؛ خارج در فاز 2 به RESCUE تکراری دوباره از وضعیت خودش ACK بدهد.

**H2 — fn164 ناقص است: آپلودی که تخلیه‌اش بیش از ۱ ثانیه طول بکشد هنوز بی‌صدا کوتاه می‌شود** (اطمینان: بالا، کد تأیید شد)
- `engine/mconn_link.go:528` (`mCloseWaitEnd = time.Second`)، `:576-577` (`waitMConnDie` و بعد `e.end()`)؛ در مقابل `engine/mconn.go:188` (`mCloseLinger = 5 * time.Second`).
- سناریو: کاربر چند MB روی لینک کند/پراتلاف می‌نویسد و می‌بندد. دنبالهٔ داده و رکورد FIN بعد از ۱ ثانیه هنوز در صف mconn است (sub پشت پنجره گیر کرده). `e.end()` همهٔ subها را با Close ساده و بدون `mRecRst` می‌بندد → خارج `errMConnSubsDied` با `rst=false` می‌گیرد (`mconn.go:1977-1980`) → مقصد FIN تمیز روی آپلود ناقص می‌بیند. همان باگ اصلی، فقط برای لینک‌های کند. آزمون `TestMConnCloseRightAfterTail` روی loopback است و این حالت را نمی‌گیرد.
- اصلاح پیشنهادی: صبر تا `mCloseLinger` + حاشیه، یا اگر `die` بسته نشده `e.end()` به‌جای Close ساده `mRecRst` بفرستد (ریست صریح بهتر از کوتاه‌شدن بی‌صداست).
- سمت خارج این الگو را ندارد (`attachExitSub` فقط روی `mc.die` می‌بندد).

### Medium

**M1 — بعد از retry در rescue خام، join قدیمیِ ساکت primary می‌ماند و بایت‌های نوشته‌شده روی آن بدون ارسال دوباره دور ریخته می‌شوند** (اطمینان: متوسط)
- `engine/mconn_rescue145.go:204-213` (`rescueRetryLocked` فقط `rrAband` می‌زند؛ نه retire، نه `prefPrimary`)، `:265-269`؛ `engine/mconn.go:820-835` (`setPrimaryLocked` اولین sub غیر retired را برمی‌دارد = join قدیمی)، `engine/mconn.go:1905` (sub با `rrAband` که می‌میرد، صفش را بدون resend رها می‌کند).
- سناریو: بعد از retry هر Write با `dupOff` فقط روی join ساکت می‌رود؛ وقتی آن لینک بالاخره می‌میرد چیزی دوباره ارسال نمی‌شود → خارج روی gap تا `mGapTimeout` (۳۰ s) می‌ماند و ریست می‌کند. مسیر records (`rrStart`) این کار را درست انجام می‌دهد (retire + `prefPrimary`).

**M2 — KEEPSTALL: chunkهای ack‌شدهٔ اتصال‌های بیکار سقف سراسری را پر می‌کنند و اتصال‌های فعال chunk حفره را دور می‌ریزند** (اطمینان: متوسط، کد تأیید شد)
- `engine/mconn_stripe_keepstall.go:156-199` (حلقهٔ `m.keptBytes > 0 && keepStallStat.kept.Load() > keepStallGlobal`)، `:86` (`keepStallGlobal = 320 MiB`)، `engine/mconn_stripe_resend.go:56` (`stripeKeepBytes = 16 MiB`)، `engine/mconn_stripe.go:381` (`stripeIdleLocked` چیزی آزاد نمی‌کند).
- سناریو: دانلود تمام می‌شود ولی اتصال (keep-alive) باز می‌ماند؛ تا ~۱۶ MiB chunk ack‌شده فقط با Write روی همان sub یا tick آزاد می‌شود و در حالت lazy tick متوقف است. حدود ۲۰ اتصال این‌چنینی روی خارج → سقف ۳۲۰ MiB → از آن به بعد **هر** Write یک اتصال فعال همهٔ chunkهای نگه‌داشته‌اش را (حفره در آخر، ولی بالاخره) دور می‌ریزد → روی مسیر تاریک resend چیزی ندارد → ریست پس از ۳۰ s (دقیقاً خرابی hs112 که KEEPSTALL برایش ساخته شد).
- اصلاح: trim با تایمر ارزان یا آزادسازی در `stripeIdleLocked`.

**M3 — retry در rescue خام بدون حلقهٔ records اتصال را می‌کشد** (اطمینان: بالا، شرط باریک)
- `engine/mconn_rescue145.go:250-263`: ارسال دوبارهٔ `[rscBi, sendOff)` از حلقهٔ records؛ آن حلقه فقط با `rescueRecOn`، حالت lite و `stN == 0` نگه داشته می‌شود. با `HS2_MCONN_RESCUE_REC=0`، بعد از رفتن به stripe/bulk، یا وقتی حلقهٔ ۳۲ KB از `rscBi` گذشته باشد → `errMConnLost` با rst=true. بهتر است در این حالت retry انجام نشود (`rscRetried` بدون اقدام) به‌جای ریست قطعی.

**M4 — join تازهٔ rescue (role 4) می‌تواند درخواست را دوباره به مقصد بفرستد** (اطمینان: متوسط)
- `engine/mconn_rescue.go:618-628`، `:688-700`؛ `engine/mconn_link.go:710-761`.
- سناریو: خارج درخواست را خدمت داده، مقصد پاسخ داده و بسته، خارج id را حذف کرده؛ ایران پاسخ را ندیده (`rawRecvd == 0`) → role 4 → خارج اتصال جدید باز می‌کند و `[0, Bi)` (مثلاً یک POST) را دوباره به مقصد جدید می‌فرستد. تحویل «حداقل یک بار» می‌شود. خارج نمی‌تواند «هرگز اولین sub را نگرفتم» را از «قبلاً تمام شد» تشخیص دهد (نیاز به tombstone کوتاه‌مدت idهای پایان‌یافته).

**M5 — نوشتنِ cut که رها شده ولی باز هم ارسال شده → `Rk > Bi` → ریست nocover** (اطمینان: متوسط-پایین؛ مقدار `n` برگشتی smux دقیق ردیابی نشد)
- `engine/mconn_rescue.go:526` (deadline `Unix(1,0)`)، `engine/mconn_raw.go:139-146` (`writeRaw`)، `mconn_rescue.go:720` (`ri > bk`)، `:766` (`ring.get(rk, Bi)`).
- از rc134 فریم رهاشده باز هم می‌رود ولی احتمالاً در `rawSent` شمرده نمی‌شود؛ گیرنده می‌تواند فراتر از `Bi` بخواند → `ring.get` شکست → ریست. باید `rk >= Bi` را «چیزی برای ارسال دوباره نیست» دانست. بیشتر در triggerهای کاذب (مسیر زنده ولی کند).

**M6 — Read می‌تواند فراتر از deadline یا برای همیشه روی `updMu` بماند** (smux؛ اطمینان: مسیر کد قطعی، احتمال وقوع متوسط)
- `third_party/smux/stream.go` حدود `:295` (`sendWindowUpdateSeq`: `s.updMu.Lock()` بدون deadline)؛ گوروتین‌های shrink در `third_party/smux/autowin.go:664` و `floor.go` (`awDropFloorLocked`) بدون deadline (اگر readDeadline تنظیم نشده باشد) قفل را تا ورود به `s.shaper` نگه می‌دارند.
- سناریو: sendLoop در `conn.Write` گیر کرده، heap شیپر به `maxShaperSize`=1024 رسیده، گوروتین shrink با `updMu` منتظر است؛ Read بعدی که باید UPD بفرستد روی `updMu` می‌ماند و `SetReadDeadline` را نادیده می‌گیرد. با `StallTimeout=0` (پیش‌فرض) و blackhole یک‌طرفه (keepalive دریافت سالم است) گیر دائمی. smux بالادستی deadline را رعایت می‌کرد.

**M7 — بستن عمدی sub در قانون ack-stall قابلیت resend همتا را چک نمی‌کند** (سازگاری نسخه؛ اطمینان: متوسط)
- `engine/mconn_stripe_keepstall.go:364-458`؛ مقایسه با `stripeEvacPeerOK` که چک می‌کند.
- با rel54c مشکلی نیست (resend از rel49). با همتای قدیمی‌تر از rel49 یا `HS2_MCONN_STRIPE_RESEND=0` بستن عمدی مثل مرگ لینک تعبیر می‌شود و ممکن است اتصال در خارج تمام شود. اصلاح: gate روی `capExtMConnResend` (bit 3).

### Low

- **L1** `third_party/smux/floor.go` `awRaiseBaseLocked`: کف ۴ (تا ۱۶) MiB پنجرهٔ heavy با bucket ~8 MiB نشست پشتیبانی نمی‌شود؛ یک جریان کف‌دار با خوانندهٔ کند می‌تواند bucket را صفر کند و `recvLoop` کل نشست را متوقف کند (کاربران سبک پشت سنگین). `HS2_STRIPE_HEAVY_FLOOR_MB=4` پیش‌فرض روشن است — ارزش یک آزمون هدفمند دارد.
- **L2** `third_party/smux/stream.go` `sendWindowUpdateSeq`: `updSent` قبل از صف‌شدن واقعی UPD جلو می‌رود؛ اگر UPD جدید روی deadline شکست بخورد، UPD قدیمی‌تر stale حساب و حذف می‌شود → از دست رفتن اطلاعات پنجره (معمولاً با UPD بعدی جبران می‌شود؛ خانوادهٔ aw163).
- **L3** `third_party/smux/stall.go` `evictUnread`: `streamLock` را می‌گیرد، درحالی‌که `recvLoop` در `cmdSYN` با همان قفل روی `chAccepts` پر بلاک است → stallWatch هم بلاک (فقط با `HS2_SMUX_STALL_S`، پیش‌فرض خاموش).
- **L4** `third_party/smux/trace.go` `Trace()`: نوشتن `s.tr` بدون قفل در برابر خواندن در `pushBytes`/`writeV2` (`engine/mconn_link.go:806`) — data race، فقط با `HS2_DL76_TRACE=1`.
- **L5** `third_party/smux/session.go` ~`:428`: SYN پس از `Close`/`clear(s.streams)` → `liveStreams` +1 بدون −1 (فقط شمارنده).
- **L6** `third_party/smux/session.go` `writeFrameQueued` + `shaper.go:16`: SmallFirst می‌تواند فریم کوچک جدید را جلوی فریم بزرگِ رهاشدهٔ همان stream بفرستد (جابه‌جایی بایت). فقط با `HS2_SMUX_SMALL_FIRST` (خارج از جدول، پیش‌فرض خاموش) و فقط اگر بعد از timeout روی همان stream دوباره نوشته شود؛ نویسنده‌های فعلی (control ping کوچک، L3 که روی خطا `markDead` می‌کند) دچار نمی‌شوند. اگر روزی روشن شود باید per-stream شود.
- **L7** `engine/mconn_rescue.go:336`, `:449-455`: `rescueSilentSet` سراسری بین LinkManagerها؛ با چند تونل در یک پروسه یکی مجموعهٔ دیگری را پاک می‌کند (شمارنده‌ها و avoid اضافه؛ rescue غلط ایجاد نمی‌شود).
- **L8** rescue در هر اتصال حداکثر یک بار (`rscPhase`/`rrPhase` هرگز به 0 برنمی‌گردند)؛ قطعی دوم روی اتصال طولانی بی‌rescue است (محدودیت طراحی؛ مستند شود).
- **L9** `engine/mconn_rescue.go:810-826`: NAK وقتی `raw1.dead` → `[Ri, bk)` هرگز نمی‌رسد → ۳۰ s توقف و سپس ریست.
- **L10** `engine/mconn_stripe.go:389` / `mconn_stripe_up.go:238`: کوچک‌شدن `oooMax` بدون `ksSyncLocked` → انحراف موقت حساب hole-room.
- **L11** `engine/mconn_stripe_share.go`: لینک‌های `stripeEvacSkip` در `w.own` شمرده می‌شوند (فقط با EVAC، خاموش)؛ `pickStripeWhy` در هر pick زیر قفل pool یک `fmt.Sprintf` می‌سازد حتی با لاگ خاموش (هزینهٔ کارایی).
- **L12** `engine/queuerun.go` `qLane.setCap` / `qCtlStat.note`: lane حذف‌شده بدون `setCap(0)` → انحراف شمارنده‌ها.
- **L13** `engine/lb_auto2.go:899-907`, `:926-931`: silent سمت edge وقتی گزارش تازه هست تقریباً همیشه حذف می‌شود (بی‌ضرر؛ توضیح کد گمراه‌کننده). `silentRuns` ممکن است در edge بی‌نهایت > 0 بماند.
- **L14** `engine/lb.go:416`, `engine/lb_silent.go:88`: مسیری که برای همیشه مرده هر ~۲ s probe می‌شود بدون backoff (محدود به `lb2SpareMax`، نشت نیست؛ SYN بی‌پایان).
- **L15** `engine/lb_uploss.go` `fold` (خاموش): برش‌های UPLOSS با گزارش OR می‌شوند بدون کف → همهٔ وزن‌ها در `steerDrawLocked` (`lb_auto2.go:1073`) صفر → steering بی‌صدا خاموش.
- **L16** `engine/lb_rescue_cut.go:85-95`: race در reset ثانیه‌ای `rescueCutAllow` (فقط دقت نرخ).
- **L17** `engine/lb_call_stuck.go:198-212`: `callStuckBusy` حالا برای همهٔ subها در هر poll ۱۰۰ ms اجرا می‌شود (syscall بیشتر).
- **L18** `cmd/hs2/tunnel.go:467-471`: کد clone روی dialer چندآی‌پی حالا IP بایند/LB را اعلام می‌کند (rt80؛ تغییر رفتار عمدی، در CHANGELOG هست).
- **L19** `engine/lb.go:164`: توضیح «under mu» برای `pmtuT` نادرست است (فقط گوروتین `LB.Run` لمسش می‌کند؛ race نیست).

### بررسی‌شده و سالم
مالکیت بافر `writeFrameCut`/`reqOwn` (rc134)؛ wakeup پنجره و `wcut`؛ ترتیب UPD (aw163) و wrap uint32 در `update()`؛ ترتیب قفل‌ها (`rmu → wmu → mu → rkMu`، `streamLock → bufferLock`)؛ اندازه‌گذاری حلقهٔ records (cd152) و `rrMemCap`/`rrBigMax`؛ پاک‌سازی `rescueConns`/`rrConns`/`deadKeys`/`memMConns`؛ سقف hole-room (۲۵۶ MiB اتصال / ۵۱۲ MiB پروسه) و kept (۹۶ MiB)؛ dedupe گوروتین‌های بستن با `s.dead`؛ آزادسازی slotهای stripe و `stripeDlActive`؛ prune نقشه‌های LB (`pmtuTrack`، `lbUpState`، `pathView`، `forget(alive)`)؛ کف‌های برش (`publishCutPaths` و `silentFilter` همه را نمی‌برند)؛ سقف accept در `stream_reverse.go`؛ تایمرها (`waitMConnDie`، read-deadline در `sendUPD`، ticker `stallWatch`).

## ۲. ایمنی نسخهٔ مختلط (در برابر rel54c)

هیچ مشکل قطعی پیدا نشد:
- smux: فرمان یا بیت نسخهٔ جدید نیست؛ autowin فقط با `Version==2`؛ SmallFirst/StallTimeout/floor/trace محلی‌اند. حذف UPD کهنه با `int32(consumed-peerConsumed)<0` در برابر هر همتا امن است. autowin یک‌طرفه در هر دو جهت درست کار می‌کند.
- rescue: role 3/4 و رکوردهای `0x0d-0x0f` فقط وقتی خارج ext bit8 اعلام کرده (`rescuePeerOK`)؛ `0x10-0x12` فقط با bit9. خارج با کلید خاموش role 3/4 را رد می‌کند. بیت‌های 8/9 با 0/2/3/4/10 تداخل ندارند. نکته: لینک join rescue دوباره از نظر cap بررسی نمی‌شود (فقط sub اول) — تا وقتی همهٔ لینک‌ها به یک build خارج می‌رسند درست است.
- stripe: bit 3 (resend)، bit 4 (overlap؛ حالا همیشه با stripe اعلام می‌شود، فقط overlap خاموش ایران رویش عمل می‌کند)، bit 10 (KEEPSTALL؛ hole-room فقط به همتای bit10). رکورد جدید نیست؛ EVAC_DOWN از CLOSE-REQ موجود استفاده می‌کند (خاموش).
- control/mt135: بیت 62 و MTU در بیت‌های 40..55 seq؛ rel54c فقط echo می‌کند؛ edge جدید با `ctrlSeqMask` ماسک می‌کند و MTU=0 را «ندارد» می‌خواند.
- استثنا: M7 (ack-stall بدون چک bit 3) — فقط برای همتای پیش از rel49 یا `RESEND=0`.

## ۳. جدول envdef (`hs2-src/envdef/envdef.go`)

- ۶۸ کلید؛ همه پس از `Activate` دوباره از طریق `envDefault`/`envdef.Get` خوانده می‌شوند (`engine/defaults22.go` `loadEnvKeys`، `loadRescue145`، `autoWinFromEnv`، `tlscarrier/dpi.go`). سه خواندن خام `os.Getenv` در `init` (`DL_FAST`، `STRIPE_FAST`، `SHARE_COPIES`) در `loadEnvKeys` دوباره خوانده می‌شوند → درست.
- سمت‌ها با منطق کد سازگارند (SILENT/QUEUE_CTL/START_DIAL/FILL_PCT روی خارج = طرف شماره‌گیر در reverse؛ RESCUE_JOINPICK/RETRY، HEAVY_FLOOR، CUT_NONEW، RETIRE_KEEP_IDLE روی ایران؛ RESCUE/SCALE/KEEPSTALL هر دو). نادرستی سمت پیدا نشد.
- پیش‌فرض‌های روشنی که این ممیزی رویشان یافته دارد: `HS2_MCONN_RESCUE_RETRY` (H1، M1، M3)، `HS2_MCONN_STRIPE_KEEPSTALL` (M2)، `HS2_STRIPE_HEAVY_FLOOR_MB` (L1)، `HS2_MCONN_RESCUE` (M4، M5، L9). هیچ‌کدام بدون شاهد آزمایشگاهی روشن نشده‌اند، ولی H1 و M2 قبل از انتشار ارزش اصلاح یا حداقل آزمون هدفمند دارند.
- توضیح `envdef.go:195` («No wire change») برای KEEPSTALL از hs133 کهنه است (ext bit10 و hole-room فقط به همتای bit10).
- کلیدهای خوانده‌شده در کد (غیرآزمونی): ۲۱۷؛ خارج از جدول: ۱۴۹؛ **بدون هیچ اشاره در README/BUILD/VALIDATION/docs/CHANGELOG: ۶۴**:
  `HS2_DL76_TRACE HS2_DUP_PATHS HS2_EXIT_SRC_ROTATE HS2_FAST_LINK_GATE HS2_FAST_LINK_INFO HS2_FAST_LINK_PAUSE HS2_L3_FEC HS2_LB_CAPX HS2_LB_FAST HS2_LB_STEER HS2_LB_STRIPE_STEER HS2_LB_STRIPE_STEER_LOG HS2_LB_TEST_NO_IDLE HS2_LB_UPLOSS HS2_LOCK_PID HS2_LOSS_RELATIVE HS2_MCONN_CALL_DIR HS2_MCONN_CALL_SUBS HS2_MCONN_RESCUE_KB HS2_MCONN_RESCUE_MB HS2_MCONN_RESCUE_MS HS2_MCONN_RESCUE_PEND_MS HS2_MCONN_RESCUE_PERSEC HS2_MCONN_RESCUE_REC_BIGMB HS2_MCONN_RESCUE_REC_KB HS2_MCONN_RESCUE_REC_MAXKB HS2_MCONN_RESCUE_REC_MB HS2_MCONN_RESCUE_REC_SECS HS2_MCONN_RESCUE_TICK_MS HS2_MCONN_RESCUE_TRACE HS2_MCONN_STRIPE_ALONE HS2_MCONN_STRIPE_BASE_MAX HS2_MCONN_STRIPE_BASE_MAX_MULTI HS2_MCONN_STRIPE_CALLFREE HS2_MCONN_STRIPE_DL_FAST_MAX HS2_MCONN_STRIPE_DL_FAST_MBIT HS2_MCONN_STRIPE_KEEPSTALL_LAZY HS2_MCONN_STRIPE_LINKBASE HS2_MCONN_STRIPE_MBIT HS2_MCONN_STRIPE_QMS HS2_MCONN_STRIPE_REL_PER HS2_MCONN_STRIPE_RES_GUARD HS2_MCONN_STRIPE_SHARE_K HS2_MCONN_STRIPE_SHARE_LANE HS2_MCONN_STRIPE_SHARE_TOTAL HS2_MCONN_STRIPE_STEP HS2_MCONN_STRIPE_UP_BASE_MAX HS2_MCONN_STRIPE_UP_LQ HS2_MEM_OBJS HS2_PICK_CLASS HS2_PROBATION_ONCE HS2_QUEUE_CTL_FLOWBLIND HS2_SHAPE_CORK HS2_SMUX_AUTOWIN_BUDGET_MB HS2_SMUX_AUTOWIN_DIV HS2_SMUX_AUTOWIN_MAX_MB HS2_SMUX_AUTOWIN_NB1 HS2_SMUX_AUTOWIN_NEIGHBOR_MB HS2_SMUX_AUTOWIN_QUEUE_PCT HS2_SMUX_STALL_DUMP HS2_STRIPE_HEAVY_FLOOR_QUS HS2_STRIPE_HEAVY_FLOOR_SOFT HS2_TUN_RCVBUF HS2_TUN_REORDER_MS`
  (بیشترشان کلید آزمایشگاهی/تنظیم ریزند؛ حداقل یک فهرست «کلیدهای آزمایشگاهی، خاموش» در README لازم است. `HS2_LB_UPLOSS` و `HS2_LB_STRIPE_STEER` ویژگی‌اند و باید نامشان بیاید.)

## ۴. مسیرهای بستن/خاموش‌شدن

- افت بی‌صدای بایت: **H2** (fn164 با سقف ۱ s)، **M1** (صف join رهاشده)، **M2** (حذف chunk حفره زیر فشار سقف سراسری).
- گیر دائمی: **M6** (Read روی `updMu`). گیر ۳۰ ثانیه‌ای و سپس ریست: **L9**، M1.
- ریست غیرلازم: **H1**، **M3**، **M5**.
- نشت گوروتین/تایمر روی مسیرهای بستن پیدا نشد (`go x.close()`/`go subGone()` با `s.dead` یکتا؛ `rescueWatch` به context وصل است؛ تایمر `waitMConnDie` Stop می‌شود).

## ۵. اصلاحات لازم در مستندات

1. `README.md` (ریشه) جدول پیش‌فرض‌ها (حدود خط 1022-1068): این کلیدهای پیش‌فرض روشن envdef نیستند → `HS2_MCONN_STRIPE_DL_FAST=1` (Iran)، `HS2_STRIPE_HEAVY_FLOOR_MB=4` (Iran)، `HS2_STRIPE_HEAVY_FLOOR_GATE=2` (Iran).
2. `README.md:803` و `hs2-src/envdef/envdef.go:195`: «No wire change, so either side may run it alone» برای KEEPSTALL کهنه است؛ از hs133 hole-room فقط به همتای ext bit10 باز می‌شود و طبق بخش rel63c در CHANGELOG بقای مسیر تاریک به هر دو طرف rel63c+ نیاز دارد.
3. سقف نرم `max_links` (`README.md:117` «caps the pool»): با `HS2_RETIRE_KEEP_IDLE` لینک‌های در حال بازنشستگی با اتصال بیکار زنده تا ۶۰ دقیقه می‌مانند و مازاد بالای max با قاعدهٔ قدیمی ۵ دقیقه‌ای پس گرفته می‌شود؛ پس تعداد لینک‌های زنده می‌تواند موقتاً از `max_links` بیشتر شود (پذیرش در reverse تا ۲×max + حاشیه). یک بند صریح «سقف نرم است» لازم است (فعلاً فقط داخل ردیف خط 806).
4. `net.ipv4.tcp_mtu_probing=1`: در README ریشه اشاره‌ای نیست؛ فقط `docs/mt135-pmtu.md` و پیام‌های `hs2 doctor`. باید در بخش tuning (حدود خط 259-267 و 1775) نوشته شود که لازم است، `hs2 run` به‌عنوان root آن را تنظیم می‌کند (`tune/tune.go:354`) مگر tuning خاموش یا `HS2_NO_TUNE` باشد، و `hs2 doctor` در غیر این صورت هشدار می‌دهد.
5. یادداشت bufferbloat سمت کاربر برای ping در speedtest: وجود ندارد. در بخش «Speed test» (حدود `README.md:1629`) و «Ping under load» (حدود خط 479) بنویسید که ping بالای کلاینت هنگام speedtest معمولاً صف لینک دسترسی خود کاربر (مودم/وای‌فای) است، نه تونل، و چگونه از هم جدا شوند (ping از سرور ایران در همان زمان).
6. autowin: در README فقط سه ردیف جدول؛ توضیح رفتار (رشد پنجره بدون صف، کوچک‌شدن با آمدن کاربر سبک، بودجهٔ `SHARE_MB`، کف heavy با `HEAVY_FLOOR_MB`/`GATE`، اثر حافظه) لازم است. متن‌های `cmd/hs2/perf.go:58-60` نقطهٔ شروع خوبی‌اند.
7. rescue: ردیف `README.md:802` هست؛ باید محدودیت‌ها اضافه شود: یک rescue در هر اتصال (L8)، RETRY فقط یک بار، احتمال ارسال دوبارهٔ درخواست در join تازه (M4)، و کلیدهای حلقه (`HS2_MCONN_RESCUE_REC*`، `RAMPCT`).
8. `CHANGELOG.md`: آخرین بخش rel63c است؛ بخش‌های rel64 (RESCUE_RETRY)، rs146 (JOINPICK/SCALE)، rel65 (cd152، sg154، race147) و rel66 (aw163/sm160، fn164، rr158..rr166، lk162/lk165) وجود ندارند؛ README این پیش‌فرض‌ها را دارد ولی CHANGELOG نه.
9. `hs2-src/README.md` (2026-09-28)، `hs2-src/BUILD.md` (2026-10-04)، `hs2-src/VALIDATION.md` (2026-10-07) پیش از جدول envdef و همهٔ این ویژگی‌ها نوشته شده‌اند؛ یا یک خط «برای رفتار فعلی README ریشه را ببینید» بگیرند یا به‌روز شوند.

## ۶. پوشش‌داده‌نشده
`queuectl.go` و `mconn_stripe_scav.go` فقط در حد hunkهای diff؛ منطق steering فراتر از diff؛ دست‌دادن بستن EVAC_DOWN در خارج؛ `doctor.go`؛ مسیر `n` برگشتی smux در M5 (نیاز به ردیابی یا آزمون). هیچ آزمونی اجرا نشد؛ برای H1، H2، M1، M2 آزمون واحد بازتولید پیشنهاد می‌شود (H2: loopback با شکل‌دهی نرخ ~۱ Mbit/s و آپلود ۲ MB و بستن فوری).
