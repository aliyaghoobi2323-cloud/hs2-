package engine

import (
	"math"
	"sync/atomic"
	"time"
)

// شمارنده‌های تشخیصی stripe (بدون کلید، ارزان: چند atomic در هر تصمیم
// دروازه که هر stripeQEvery یک‌بار برای هر اتصال گرفته می‌شود). در
// status.json هر دو سمت دیده می‌شوند: سمت خارج دروازهٔ دانلود، سمت ایران
// دروازهٔ آپلود (HS2_MCONN_STRIPE_UP) و قاعدهٔ رشد.

// علت‌های تصمیم دروازه (هر تصمیم دقیقاً یکی)
const (
	gwGrow  = iota // پهنا یکی بیشتر شد
	gwCut          // صف بیش از هدف: نصف شد (یا صفر ماند)
	gwBand         // صف بین هدف/۲ و هدف: نه رشد نه برش
	gwHold         // صف کم ولی هنوز در نگه‌داشت پس از برش/رشد قبلی
	gwToken        // صف کم ولی سطل سراسری رشد خالی
	gwFull         // پهنا = همهٔ subهای زنده (باز کامل)
	gwN
)

var stripeDiag struct {
	dec    [gwN]atomic.Uint64 // تصمیم‌ها به تفکیک علت (از آغاز)
	closed atomic.Uint64      // تصمیم‌هایی که پس از آن پهنا ۰ ماند با sub زنده
	noMeas atomic.Uint64      // تصمیم بی‌اندازه (هیچ لینکی ردیابی نمی‌شد)
	subB   atomic.Uint64      // بایت‌هایی که روی subهای stripe (نه primary) رفت

	// بازهٔ جاری (status هر بار Swap می‌کند): میانگین/بیشینهٔ پهنا
	iDec, iW, iLive atomic.Uint64
	iWMax           atomic.Int64
	// آخرین مقدارها (میکروثانیه)
	lastQRaw, lastQ, lastBase atomic.Int64
	lastBusy                  atomic.Int64 // لینک‌های پرکار دیگر در آخرین اندازه
	// قاعدهٔ رشد ایران
	growUnused atomic.Uint64 // رشد رد شد: sub بی‌استفاده (دروازهٔ فرستنده بسته)
	growRate   atomic.Uint64 // نرخ زیر قاعدهٔ رشد
	pickNone   atomic.Uint64 // لینکی برای sub تازه پیدا نشد
	// ورودی تصمیم‌های بازه: کمینه و بیشینهٔ صف خام (µs) برای دیدن نوسان
	iQMin, iQMax atomic.Int64
	stepRebase   atomic.Uint64 // HS2_MCONN_STRIPE_STEP: پله‌های پذیرفته
	jumpHold     atomic.Uint64 // HS2_MCONN_STRIPE_JUMP_MS: تصمیم‌های در مهلت پرش

}

func init() { stripeDiag.iQMin.Store(math.MaxInt64) }

func casMax(a *atomic.Int64, v int64) {
	for {
		o := a.Load()
		if v <= o || a.CompareAndSwap(o, v) {
			return
		}
	}
}

func casMin(a *atomic.Int64, v int64) {
	for {
		o := a.Load()
		if v >= o || a.CompareAndSwap(o, v) {
			return
		}
	}
}

// stripeDiagGate (زیر m.mu): یک تصمیم دروازه.
func stripeDiagGate(why, w, live int, qRaw, q, base time.Duration, busy int) {
	d := &stripeDiag
	d.dec[why].Add(1)
	if w == 0 && live > 0 {
		d.closed.Add(1)
	}
	d.iDec.Add(1)
	d.iW.Add(uint64(w))
	d.iLive.Add(uint64(live))
	casMax(&d.iWMax, int64(w))
	d.lastQRaw.Store(qRaw.Microseconds())
	d.lastQ.Store(q.Microseconds())
	d.lastBase.Store(base.Microseconds())
	d.lastBusy.Store(int64(busy))
	casMax(&d.iQMax, qRaw.Microseconds())
	casMin(&d.iQMin, qRaw.Microseconds())
}

// StripeSnap: وضعیت stripe برای hs2 status.
type StripeSnap struct {
	Active, RxActive                    int32  // اتصال‌های دارای جای stripe (ایران / گیرندهٔ خارج)
	Subs, Bytes, SubBytes, Refused      uint64 // از آغاز
	Cuts                                uint64
	Grow, Cut, Band, Hold, Token, Full  uint64 // تصمیم‌های دروازه به تفکیک علت
	Closed, NoMeasure                   uint64
	GrowUnused, GrowRate, PickNone      uint64
	FastChained, FastEarly, FastRetry   uint64 // HS2_MCONN_STRIPE_FAST (mconn_stripe_fast.go)
	FastReserved                        int64  // up56: HS2_MCONN_STRIPE_FAST_RESERVE idle links
	FastRefills, FastHsKicks, FastBumps uint64 // up56
	// بازهٔ گذشته (از خواندن قبلی)
	Decisions          uint64
	WAvg, LiveAvg      float64
	WMax               int64
	QRawMinUs, QRawMax int64
	// آخرین تصمیم
	QRawUs, QUs, BaseUs, BaseLearnedUs int64
	StepRebase                         uint64 // HS2_MCONN_STRIPE_STEP
	JumpHold                           uint64 // HS2_MCONN_STRIPE_JUMP_MS
	BusyLinks                          int64
}

// StripeObserved: شمارنده‌ها؛ میانگین‌ها و بیشینه‌های بازه با هر فراخوانی صفر می‌شوند.
func StripeObserved() StripeSnap {
	d := &stripeDiag
	s := StripeSnap{
		Active: stripeStat.active.Load(), RxActive: stripeStat.rxActive.Load(),
		Subs: stripeStat.subs.Load(), Bytes: stripeStat.bytes.Load(), SubBytes: d.subB.Load(),
		Refused: stripeStat.refused.Load(), Cuts: stripeStat.cuts.Load(),
		Grow: d.dec[gwGrow].Load(), Cut: d.dec[gwCut].Load(), Band: d.dec[gwBand].Load(),
		Hold: d.dec[gwHold].Load(), Token: d.dec[gwToken].Load(), Full: d.dec[gwFull].Load(),
		Closed: d.closed.Load(), NoMeasure: d.noMeas.Load(),
		GrowUnused: d.growUnused.Load(), GrowRate: d.growRate.Load(), PickNone: d.pickNone.Load(),
		FastChained: stripeFastStat.chained.Load(), FastEarly: stripeFastStat.early.Load(), FastRetry: stripeFastStat.retries.Load(),
		FastReserved: stripeFastStat.reserved.Load(), FastRefills: stripeFastStat.refills.Load(), FastHsKicks: stripeFastStat.hsKicks.Load(), FastBumps: stripeFastStat.bumps.Load(),
		QRawUs: d.lastQRaw.Load(), QUs: d.lastQ.Load(), BaseUs: d.lastBase.Load(), BusyLinks: d.lastBusy.Load(),
	}
	stripePathQ.mu.Lock()
	s.BaseLearnedUs = stripePathQ.bq.Microseconds()
	s.StepRebase = d.stepRebase.Load()
	s.JumpHold = d.jumpHold.Load()
	stripePathQ.mu.Unlock()
	n := d.iDec.Swap(0)
	w, l := d.iW.Swap(0), d.iLive.Swap(0)
	s.WMax = d.iWMax.Swap(0)
	s.QRawMax = d.iQMax.Swap(0)
	if mn := d.iQMin.Swap(math.MaxInt64); mn != math.MaxInt64 {
		s.QRawMinUs = mn
	}
	s.Decisions = n
	if n > 0 {
		s.WAvg, s.LiveAvg = float64(w)/float64(n), float64(l)/float64(n)
	}
	return s
}

// ---- اصلاح آزمایشی (پیش‌فرض خاموش) -----------------------------------------

// stripeQWin: HS2_MCONN_STRIPE_QWIN_MS (پیش‌فرض ۰: خاموش). صف هر لینک به‌جای
// srtt − min_rtt کرنل (کمینهٔ ۵ دقیقه‌ای، tcp_min_rtt_wlen) نسبت به کمینهٔ
// srtt همان لینک در این پنجرهٔ کوتاه حساب می‌شود. نوسان مسیر (مسیری که چند
// دقیقه پیش کوتاه‌تر بود) min_rtt کرنل را پایین نگه می‌دارد و فاصلهٔ ثابتی
// بزرگ‌تر از سقف پایه (۴ms) به همهٔ لینک‌های آن مسیر می‌دهد که صف نیست و
// دروازه را برای همیشه می‌بندد. صفی که خود stripeها بسازند در پنجره‌ای چند
// ثانیه‌ای کمینه نمی‌شود، چون BBR لینک‌های دیگر هر ۱۰ ثانیه صف را خالی
// می‌کند (PROBE_RTT) و srtt آن‌ها در آن لحظه به پایهٔ واقعی برمی‌گردد.
var stripeQWin time.Duration // HS2_MCONN_STRIPE_QWIN_MS (loadEnvKeys)

// qwinMin: کمینهٔ دو-سطلی srtt یک لینک (هر سطل نیم پنجره).
type qwinMin struct {
	cur, prev uint32
	at        time.Time
}

// note: srtt تازه را ثبت می‌کند و کمینهٔ پنجره را برمی‌گرداند.
func (w *qwinMin) note(now time.Time, rtt uint32) uint32 {
	if w.at.IsZero() || now.Sub(w.at) >= stripeQWin/2 {
		w.prev, w.cur, w.at = w.cur, rtt, now
		if w.prev == 0 {
			w.prev = rtt
		}
	} else if rtt < w.cur {
		w.cur = rtt
	}
	return min(w.cur, w.prev)
}
