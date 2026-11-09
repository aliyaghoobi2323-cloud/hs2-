package engine

import (
	"os"
	"slices"
	"time"
)

// ---- اصلاح آزمایشی dl-gate15 (پیش‌فرض خاموش) -------------------------------
//
// stripeLinkBase: HS2_MCONN_STRIPE_LINKBASE=1. صف هر لینک پیش از میانه نسبت به
// پایهٔ نوفهٔ خود همان لینک سنجیده می‌شود: صدک ۱۰ صف همان لینک در ۳۰ ثانیهٔ
// اخیر (فقط وقتی پرکار بود)، با سقف stripeBaseMax. در دروازهٔ کنونی پایه یک
// عدد برای کل مسیرهاست (صدک ۱۰ میانه)، پس وقتی یک مسیر لرزش دارد و دیگری
// ندارد، میانهٔ لینک‌ها بسته به این‌که در آن ۱۰۰ms کدام لینک‌ها پرکار بودند
// بین دو جمعیت می‌پرد و از پایه+هدف بالا می‌زند: برش بی‌آن‌که صفی ساخته شده
// باشد. با پایهٔ هر لینک، لرزش ثابت هر مسیر از لینک‌های خودش کم می‌شود و فقط
// افزایش واقعی (صف تازه) می‌ماند. لینکی که هنوز پایه ندارد (کمتر از
// stripeBaseMin اندازهٔ پرکار) در میانه شمرده نمی‌شود، مگر هیچ لینکی پایه
// نداشته باشد. فقط دروازهٔ دانلود (سمت خارج)؛ آپلود ایران (light) دست نمی‌خورد.
var stripeLinkBase = os.Getenv("HS2_MCONN_STRIPE_LINKBASE") == "1"

// stripeLinkBaseEvery: پایهٔ هر لینک هر این‌قدر اندازه یک‌بار از نو (ارزان).
const stripeLinkBaseEvery = 10

// linkBase: تاریخچهٔ صف یک لینک (زیر stripeQ.mu).
type linkBase struct {
	hist []time.Duration
	i, n int
	b    time.Duration
	ok   bool
}

// note: یک اندازهٔ پرکار.
func (lb *linkBase) note(q, cap_ time.Duration) {
	if len(lb.hist) < stripeBaseN {
		lb.hist = append(lb.hist, q)
	} else {
		lb.hist[lb.i] = q
		lb.i = (lb.i + 1) % stripeBaseN
	}
	lb.n++
	if len(lb.hist) < stripeBaseMin {
		return
	}
	if lb.ok && lb.n%stripeLinkBaseEvery != 0 {
		return
	}
	s := slices.Clone(lb.hist)
	slices.Sort(s)
	lb.b = min(s[len(s)*stripeBasePct/100], cap_)
	lb.ok = true
}

// excess: صف لینک بالای پایهٔ خودش؛ false: پایه هنوز نیست.
func (lb *linkBase) excess(q time.Duration) (time.Duration, bool) {
	if !lb.ok {
		return 0, false
	}
	return max(0, q-lb.b), true
}
