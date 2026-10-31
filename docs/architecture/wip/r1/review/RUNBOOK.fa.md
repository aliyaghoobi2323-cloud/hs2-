# دستورالعمل نصب میدانی R1 روی دو سرور (ایران و خارج)

بازبینی ج (نصب، ارتقا، پیکربندی و عملیات) برای فاز R1. گزارش بازبینی: `C-ops.fa.md` کنار همین پرونده.

- **تونل:** l3mtcp معکوس. لبهٔ ایران (`"mode": "dial"`، `"reverse": true`) گوش می‌دهد. خروجی خارج (`"mode": "listen"`، `"reverse": true`) شماره می‌گیرد. پنل روی خارج است. واحد `hs2` روی هر دو، با پیکربندی `/etc/hs2/config.json`.
- **نام‌ها:** در این سند `<IRAN>` و `<KHAREJ>` نام میزبان دو سرور در ssh رایانهٔ صاحب پروژه‌اند. هیچ نشانی، کلید یا پیوندی این‌جا نیامده است.
- **قاعدهٔ پایه:** R1 با ساخت تولید (`1c23629533c9`) وصل نمی‌شود. پس دو سرور باید در **یک پنجره** عوض شوند. اول سری که گوش می‌دهد (ایران)، و **بی‌درنگ** پس از آن سری که شماره می‌گیرد (خارج). شماره‌گیری که سر دیگر ردش کند ۸ ثانیه صبر می‌کند و این درنگ تا ۶۰ ثانیه دو برابر می‌شود.
- **هشدار: `hs2-menu` → «5) Upgrade» (و `u` در منوی هر تونل) را یک‌طرفه نزنید.** R1 حالا روی `main` است، پس این منو نسخهٔ R1 را نصب می‌کند (یافتهٔ ج۱۱ پیش از رفتن R1 به `main` عکس این بود). روی سروری که هنوز ساخت پیش از R1 دارد، زدنش فقط روی یک سر تونل را قطع می‌کند، چون R1 با آن ساخت‌ها وصل نمی‌شود، و قطعی تا عوض شدن سر دیگر می‌ماند. این منو زمان‌سنج دوطرفه و بازگشت خودکار ندارد. ارتقا و بازگشت فقط با `hs2-safe-apply` و همین دستورالعمل.
- **روش اجرا:** هر دو سرور با زمان‌سنج گذرای systemd در **لحظه‌ای مطلق** که از پیش تعیین شده عوض می‌شوند: ایران در `T`، خارج در `T + 0.3 s`.
  - این روش به SSH در لحظهٔ تغییر بسته نیست. چرا مهم است: وقتی راه مستقیم رایانه به خارج بسته است، ssh رایانه از راه ایران و **درون همین تونل** به خارج می‌رسد. در این بازبینی هم یک بار چنین شد. در میانهٔ تغییر تونل قطع است، پس دستور دوم از راه SSH به خارج نمی‌رسد.
  - زمان‌سنج‌ها روی سرور خارج سنجیده شدند و با `AccuracySec=1ms` ‏۱۸ تا ۳۲ میلی‌ثانیه پس از لحظهٔ تعیین‌شده اجرا شدند.

## ۰. آنچه پیش از روز نصب باید روشن باشد

1. **نامزد تأییدشده:** شناسهٔ ثبتی که سرپرست برای میدان تأیید کرده است. باینری همین بازبینی از `c0111e0` ساخته شد، با sha256 `9856c2f91654…` برای amd64. اگر ثبت‌های این شاخه (`r1-review-ops`) هم پذیرفته شوند، باینری فقط یک سطر لاگ تازه دارد (`d1c30e0`) و شناسه و sha256 آن فرق می‌کند. همان sha256ی که ساختید در گزارش میدان ثبت شود.
2. **بار کاربران:** پنجره در ساعتی کم‌کاربر باشد. قطعی مورد انتظار حدود ۱٫۲ ثانیه است و همهٔ اتصال‌های جاری کاربران یک بار می‌افتند، مانند هر راه‌اندازی دوباره.
3. **شمار لینک:** امروز خارج ۱۲ لینک دارد، با سقف خودکار ۱۱۴ که ایران تعیین می‌کند. R1 در استخری با بیش از ۵ لینک **۴ لینک QUIET** هم کنار لینک‌های BULK دارد. پس در اندازهٔ امروز ۱۶ اتصال TLS، و در بیکاری (`min_links` ۲) ‏۶ اتصال.
   - اگر «۶ لینک» برای میدان خواسته شده است، یعنی تغییر `min_links`/`max_links` در پیکربندی. این تغییر را **در پنجرهٔ جدا** انجام دهید، نه هم‌زمان با عوض کردن باینری؛ در معکوس، سقف ایران تعیین‌کننده است.

## ۱. ساخت و پخش (یک روز پیش)

**روی خارج** (ساخت روی هسته‌های آزمون؛ به سرویس تولید دست نمی‌زند):

```bash
cd /root/hs2-wt/<worktree-of-approved-commit>/hs2-src
export PATH=/usr/local/go/bin:$PATH
git status --short                      # باید خالی باشد
/root/r1-cpu.sh make release DIST=/root/r1-field/dist
/usr/local/go/bin/go version -m /root/r1-field/dist/hs2-linux-amd64 | grep -E 'vcs.revision|vcs.modified|CGO_ENABLED|trimpath'
/root/r1-field/dist/hs2-linux-amd64 version     # [build <rev12> <date>] بی '+'
mkdir -p /root/r1-field/stage-r1 /root/r1-field/stage-prod /root/r1-field/backup
install -m 755 /root/r1-field/dist/hs2-linux-amd64 /root/r1-field/stage-r1/hs2
install -m 755 /usr/local/bin/hs2 /root/r1-field/stage-prod/hs2
install -m 755 ../scripts/hs2-safe-apply /root/r1-field/hs2-safe-apply
sha256sum /root/r1-field/stage-r1/hs2 /root/r1-field/stage-prod/hs2 /root/r1-field/hs2-safe-apply
```

- `make release` دو بار پشت سر هم همان sha256 را داد. باینری ایستا است (`not a dynamic executable`)، و ابزار، وابستگی‌ها و پرچم‌هایش همان باینری تولید است.
- **در `stage-r1` و `stage-prod` فقط پروندهٔ `hs2` باشد.** اگر `config.json` یا `dropin.conf` آن‌جا باشد، `hs2-safe-apply` آن را هم اعمال می‌کند.
- **`/root/r1-field/hs2-safe-apply` را به کار ببرید، نه `/usr/local/sbin/hs2-safe-apply`.** نسخهٔ نصب‌شده روی خارج کهنه است (۲۵ اکتبر): `HS2_SAFE_PEER` را اجباری می‌خواهد، `HS2_SAFE_PEER_WAIT` را نمی‌خواند، و بی آن هیچ کاری نمی‌کند. نسخهٔ ایران دانسته نیست.

**به ایران** (از رایانه؛ خارج کلید ssh ایران را ندارد):

```bash
scp <KHAREJ>:/root/r1-field/stage-r1/hs2 ./hs2-r1
scp <KHAREJ>:/root/r1-field/hs2-safe-apply ./hs2-safe-apply
ssh <IRAN> 'mkdir -p /root/r1-field/stage-r1 /root/r1-field/stage-prod /root/r1-field/backup'
scp ./hs2-r1 <IRAN>:/root/r1-field/stage-r1/hs2
scp ./hs2-safe-apply <IRAN>:/root/r1-field/hs2-safe-apply
ssh <IRAN> 'chmod 755 /root/r1-field/stage-r1/hs2 /root/r1-field/hs2-safe-apply &&
  install -m 755 /usr/local/bin/hs2 /root/r1-field/stage-prod/hs2 &&
  sha256sum /root/r1-field/stage-r1/hs2 /root/r1-field/stage-prod/hs2 /root/r1-field/hs2-safe-apply'
```

- sha256 باینری R1 و `hs2-safe-apply` روی دو سرور باید یکی باشد.
- sha256 `stage-prod/hs2` هر سرور را ثبت کنید. روی خارج `bd7966970bd0…` است (ساخت `1c23629533c9`). اگر ایران چیز دیگری دارد، بازگشت ایران به همان باینری خودش است، و این را بنویسید.

## ۲. پشتیبان (یک روز پیش، و دوباره پیش از پنجره)

روی **هر دو** سرور:

```bash
B=/root/r1-field/backup/$(date -u +%Y%m%d-%H%M%S); mkdir -p "$B" && chmod 700 "$B"
cp -a /usr/local/bin/hs2 "$B/hs2"
cp -a /etc/hs2 "$B/etc-hs2"
systemctl cat hs2 > "$B/unit.txt"
[ -d /etc/systemd/system/hs2.service.d ] && cp -a /etc/systemd/system/hs2.service.d "$B/dropins"
sha256sum "$B/hs2" > "$B/sha256.txt"; ls -la "$B"
```

- `hs2-safe-apply` جدا از این، باینری، پیکربندی واحد و کل پوشهٔ drop-in را در `/root/hs2-backups/apply-*` نگه می‌دارد.
- از این بازبینی به بعد، اگر نتواند پشتیبان بسازد هیچ کاری نمی‌کند (`REFUSED backup`).

## ۳. وارسی‌های پیش از پنجره (فقط خواندنی، روی هر دو)

| # | فرمان | انتظار |
|---|---|---|
| 1 | `systemctl show hs2 -p ActiveState -p MainPID -p NRestarts -p ExecStart --no-pager` | `active`؛ `ExecStart` با `-c /etc/hs2/config.json`؛ `NRestarts` را بنویسید |
| 2 | `/usr/local/bin/hs2 version` و `/root/r1-field/stage-r1/hs2 version` | اولی `[build 1c23629533c9 …]`، دومی شناسهٔ نامزد |
| 3 | `/root/r1-field/stage-r1/hs2 check -c /etc/hs2/config.json; /usr/local/bin/hs2 check -c /etc/hs2/config.json` | هر دو `config OK` (هشدارها را بخوانید). اگر R1 خطا (`ERROR`) بدهد و تولید نه، **دست نگه دارید**. `hs2-safe-apply` آن سرور را رد می‌کند (`REFUSED config-check-new`)، و سرور دیگر تنها می‌رود: ۱۲۰ ثانیه قطعی و بازگشت (سناریوی `up-erefuse`) |
| 4 | `ping -c 3 -W 2 "$(sed -n 's/.*"peer_ip"[^"]*"\([^"]*\)".*/\1/p' /etc/hs2/config.json)"` | پاسخ می‌آید. همین آزمونی است که `hs2-safe-apply` می‌کند |
| 5 | `timedatectl show -p NTPSynchronized --value` و `timedatectl timesync-status 2>/dev/null \| grep -i offset` | `yes` و اختلاف زیر ۵۰ میلی‌ثانیه. از رایانه: `for h in <IRAN> <KHAREJ>; do ssh $h 'date +%s.%N'; done` (تقریبی) |
| 6 | `df -h /usr/local/bin /root /etc` | جای پشتیبان (چند ده MB) |
| 7 | `systemctl list-units --all --no-pager 'hs2*'` | فقط `hs2` (و روی خارج `hs2-mon`). اگر تونل دیگری (`hs2-<name>`) هست، آن هم در راه‌اندازی بعدی‌اش R1 می‌شود، چون باینری مشترک است |
| 8 | `ls /etc/systemd/system/hs2.service.d 2>&1` | drop-inهای موجود را بدانید (امروز روی خارج هیچ) |
| 9 | `systemd-analyze calendar 'YYYY-MM-DD HH:MM:SS.300 UTC'` | `Normalized form` با `.300000` |
| 10 | از رایانه: `timeout 6 bash -c '</dev/tcp/<KHAREJ-IP>/22' && echo direct` | بدانید راه مستقیم باز است یا نه. زمان‌سنج به آن بسته نیست، ولی دیدن خارج در پنجره هست |

## ۴. آرام کردن خارج (پیش از پنجره و در کل سنجش میدانی)

خارج فرستندهٔ دانلود است و آزمون‌ها و چیدمان عامل‌ها روی همان میزبان اجرا می‌شوند. کار عامل‌ها بار CPU و I/O می‌گذارد، و hs2 هم در لاگ «server is saturated» می‌نویسد.

```bash
/root/r1-quiet.take.sh field-r1 "R1 field install and measurement"   # تا نوبتش برسد صبر می‌کند و می‌نویسد taken
systemctl freeze 'r1cpu-*.scope'
systemctl show -p Id -p FreezerState 'r1cpu-*.scope'                  # همه frozen
ps -eo pid,args | grep -E '[h]s2[^ ]* run -c' | grep -v ' -c /etc/hs2/'   # هیچ hs2 آزمایشگاهی (باید خالی باشد)
```

- `r1-cpu.sh`ای که **پس از** انجماد آغاز شود منجمد نیست، چون scope تازه‌ای می‌سازد. پس در کل پنجره این را هم بگذارید و پس از پایان بایستانید:

  ```bash
  systemd-run --unit=r1-field-freezer /bin/sh -c "while sleep 10; do systemctl freeze 'r1cpu-*.scope' 2>/dev/null; done"
  ```

- **پس از سنجش:**

  ```bash
  systemctl stop r1-field-freezer
  systemctl thaw 'r1cpu-*.scope'
  grep -q '^field-r1 ' /root/r1-quiet.lock && rm -f /root/r1-quiet.lock
  ```

  آزمونی که هم‌زمان با انجماد بود ممکن است با مهلت شکست خورده باشد. باید دوباره اجرا شود (گزارش `cpu`، بخش ۷).

## ۵. پنجرهٔ نصب

**لحظهٔ `T`** را به UTC و دست‌کم ۱۰ دقیقه بعد بگذارید، با ثانیهٔ گرد (مثلاً `2026-11-01 02:30:00`).

**T − ۱۰ دقیقه:** پشتیبان بخش ۲ را دوباره بگیرید، وارسی‌های بخش ۳ را دوباره ببینید، و خارج را مانند بخش ۴ آرام کنید.

**T − ۵ دقیقه:** دو زمان‌سنج را بگذارید.

ایران، در `T`:

```bash
systemd-run --unit=hs2-r1-up --on-calendar='YYYY-MM-DD HH:MM:SS.000 UTC' \
  --timer-property=AccuracySec=1ms --timer-property=RandomizedDelaySec=0 \
  --setenv=HS2_SAFE_UNIT=hs2 --setenv=HS2_SAFE_PEER_WAIT=120 \
  /root/r1-field/hs2-safe-apply /root/r1-field/stage-r1
```

خارج، در `T + 0.3 s` (همان فرمان، فقط زمانش):

```bash
systemd-run --unit=hs2-r1-up --on-calendar='YYYY-MM-DD HH:MM:SS.300 UTC' \
  --timer-property=AccuracySec=1ms --timer-property=RandomizedDelaySec=0 \
  --setenv=HS2_SAFE_UNIT=hs2 --setenv=HS2_SAFE_PEER_WAIT=120 \
  /root/r1-field/hs2-safe-apply /root/r1-field/stage-r1
```

- **وارسی روی هر دو:** `systemctl list-timers --all --no-pager hs2-r1-up.timer`. ستون NEXT باید همان `T` (و `T + 0.3`) باشد.
- **لغو پیش از `T`:** `systemctl stop hs2-r1-up.timer` روی **هر دو**. اگر فقط یکی لغو شود، دیگری تنها می‌رود: ۱۲۰ ثانیه قطعی و بازگشت خودکار.
- **نام‌ها:** نام ثابت `hs2-r1-up` عمدی است: زمان‌سنج دوم با همین نام پذیرفته نمی‌شود («already exists»)، پس دو بار گذاشتن ممکن نیست.
  - اگر `hs2-safe-apply` با کد ۱ پایان یافت (REVERTED یا REFUSED)، سرویس گذرا در حالت failed می‌ماند. پیش از استفادهٔ دوباره از همان نام: `systemctl reset-failed hs2-r1-up.service`.
  - برای بازگشت نام دیگری به کار ببرید (`hs2-r1-back`، بخش ۸).

**چرا ۰٫۳ ثانیه** (درون `sandbox.sh`، بخش ۱۰). «فاصلهٔ خدمت» از نخستین راه‌اندازی دوباره تا نخستین اتصال تازه‌ای است که پس از هر دو پاسخ گرفت:

| ترتیب | فاصلهٔ خدمت |
|---|---|
| هر دو با هم | 0.9 s |
| ایران اول، 0.3 s | 1.2 s |
| ایران اول، 0.7 s | 1.2 تا 1.3 s |
| خارج اول، 0.3 s | 0.9 s |
| خارج اول، 0.6 s | 1.5 s |
| خارج اول، 1.0 s | **9.3 s** |
| خارج اول، 2.0 s | **8.3 s** |

- اگر خارج بیش از ~۰٫۸ ثانیه زودتر عوض شود، R1 خارج پیش از رفتن تولید ایران به آن می‌رسد. رد می‌شود و ۸ ثانیه صبر می‌کند.
- ایران اول هیچ جریمه‌ای ندارد؛ فقط خود فاصله و ~۰٫۵ ثانیه وصل شدن.
- پس ۰٫۳ ثانیه به سود ایران، در برابر اختلاف ساعت (چند میلی‌ثانیه با NTP) و ناهم‌سانی زمان آماده‌سازی `hs2-safe-apply` (رونوشت پشتیبان، `version`، `check`، `daemon-reload`) جا می‌گذارد. قطعی مورد انتظار **~۱٫۲ ثانیه** است.
- `hs2-safe-apply` لحظهٔ راه‌اندازی هر سرور را با میلی‌ثانیه در لاگش می‌نویسد (`applied; restarting …`). فاصلهٔ واقعی را پس از پنجره از همان‌جا بخوانید.

**در `T`:** هیچ فرمانی لازم نیست. خط زمانی مورد انتظار (sandbox):

| زمان | چه رخ می‌دهد |
|---|---|
| T + ~0.05 s | ایران راه‌اندازی دوباره می‌شود |
| T + ~0.35 s | خارج راه‌اندازی دوباره می‌شود |
| T + ~1.2 s | تونل برمی‌گردد (sandbox: ۱٫۱۵ ثانیه پس از راه‌اندازی ایران) |
| T + 60 s | هر دو `hs2-safe-apply` وارسی می‌کنند: `active` و `NRestarts` بی‌تغییر، سپس پینگ همتا |
| T + ~61 s | `/root/hs2-safe-apply.last` روی هر دو: `OK /root/hs2-backups/apply-…` |

## ۶. آنچه در دقیقه‌های نخست دیده می‌شود

از رایانه (ایران همیشه مستقیم است؛ خارج اگر راه باشد):

```bash
ssh <IRAN> 'tail -f /root/hs2-safe-apply.log' &
ssh <IRAN> 'journalctl -u hs2 -f -o short-precise'
```

**لاگ ایران، در ثانیهٔ نخست:**
- `stream edge (reverse): listening for kharej links on …`
- `user port … open (tcp)`
- `mtcp: QUIET link q0 up from … (now 1 QUIET)` تا `q3` (چهار QUIET)
- `mtcp: reverse link N up from … (now N)`

**لاگ خارج:**
- `mtcp: exit pool starting N links`
- `exit link up to edge (slot …)`
- `mtcp: exit pool target N+4 links (edge asked; …)`

**`hs2 status` در T + 5 ثانیه** (روی هر دو):

```bash
hs2 status -c /etc/hs2/config.json | grep -E '^ *links:|protocol:|place:|events since|cpu:'
```

| سطر | ایران | خارج |
|---|---|---|
| `links:` | `N up` (فقط BULK) | `N+4 up` |
| `protocol:` | `protocol: v3 level 2 (peer levels 1-2, caps 0x…); refused v2 peers: 0` | `protocol: v3 level 2 (…)` |
| `place:` | `quiet 4 of 4 links, bulk N links; … on bulk (no quiet up) X` | — |

- در `place:` شمار «on bulk (no quiet up)» پس از ثانیهٔ نخست نباید بالا برود.
- **یک خط در هر دقیقه:** `events in the last minute: …`. فقط وقتی چیزی جابه‌جا شده باشد نوشته می‌شود؛ تولید امروز ~۵۲ سطر در ساعت دارد.

**این‌ها نباید دیده شوند:**
- ایران: `peer runs protocol v2; upgrade both servers`. یعنی خارج روی تولید است.
- خارج: `the other server answered as a plain website, not as the tunnel … or it runs an older hs2`. یعنی ایران روی تولید است.
- هر دو: `panic`، `fatal`، و `tuning: HS2_TUNE_… ignored` (کلید نادرست؛ فقط با ثبت `d1c30e0`).

**در ۱۰ دقیقهٔ نخست، هر ۲ دقیقه:**

```bash
systemctl show hs2 -p NRestarts -p MainPID --value          # بی‌تغییر
ps -o rss=,pcpu= -p "$(systemctl show -p MainPID --value hs2)"
journalctl -u hs2 --since -2min --no-pager | wc -l           # چند سطر، نه صدها
cat /root/hs2-safe-apply.last
```

- پینگ درون تونل و یک آزمون واقعی با کلاینت (پنل، صفحهٔ وب، تماس) را هم ببینید.

## ۷. نشانه‌های بازگشت

**خودکار:** `hs2-safe-apply` هر سرور خودش برمی‌گردد اگر:
- hs2 در ثانیهٔ ۶۰ پایدار نباشد (`active` نباشد یا `NRestarts` عوض شده باشد)؛ یا
- همتا تا ثانیهٔ ۱۲۰ به پینگ پاسخ ندهد.

**یک دام:** هر سرور **جدا** تصمیم می‌گیرد. اگر یکی برگردد و دیگری پیش از آن `OK` گفته باشد، دو سر دو نسخهٔ ناسازگار دارند و تونل **تا ابد** قطع می‌ماند.
- سناریوی `up-xflapn`: خارج یک بار افتاد و برگشت، ایران `OK` ماند، و تونل هرگز برنگشت.
- `hs2-safe-apply` خط `REVERTED` را فقط **پس از** ۱۲۰ ثانیه انتظار خودش در `.last` می‌نویسد. پس **`REVERTING` را در لاگ** ببینید، نه `.last`.

**بازگشت دستی، بی‌درنگ، اگر یکی از این‌ها رخ داد:**
1. **نسخه‌ها ناهم‌خوان شدند:**
   - در لاگ یک سرور `REVERTING` آمد و دیگری `OK` است؛ یا
   - ایران `peer runs protocol v2` می‌نویسد؛ یا
   - خارج `answered as a plain website` می‌نویسد.
2. تونل با هر دو سرِ R1 بیش از ۳۰ ثانیه قطع است و برنمی‌گردد.
3. کاربران آسیب می‌بینند: پنل از راه تونل نمی‌رسد، صفحه‌ها باز نمی‌شوند، یا تماس قطع می‌شود.
   - اگر فقط تماس و وب زیر اتلاف مسیر خراب است (R2-13 تیم قرمز)، اول کلید `HS2_TUNE_QUIET_CC=bbr` روی هر دو را بسنجید (drop-in، بخش ۸). بهایش بازی بدتر است. اگر بس نبود، بازگشت کامل.
4. `NRestarts` بالا می‌رود (پس از دقیقهٔ نخست، دیگر خودکار برنمی‌گردد).
5. RSS در ۳۰ دقیقه به بیش از دو برابر تولید رسید، یا CPU بی‌دلیل بالاست.
6. لاگ سیل‌آسا است (بیش از ~۶۰ سطر در دقیقه، پیوسته).

**جدول تصمیم، وقتی خارج در دسترس نیست:** ایران همیشه در دسترس است، پس **ایران را هم‌نسخهٔ خارج کنید** تا تونل برگردد.

| آنچه روی ایران دیده می‌شود | خارج روی | کار روی ایران |
|---|---|---|
| ایران R1 (`hs2 version` شناسهٔ نامزد) و لاگ `peer runs protocol v2; upgrade both servers` | تولید | بازگشت ایران، بی‌درنگ (`stage-prod`) |
| ایران تولید (`1c23629533c9`)، بی لینک، و لاگ ایران ساکت | احتمالاً R1 | دوباره R1 روی ایران (`stage-r1`)؛ تونل برمی‌گردد، سپس اگر خواستید بازگشت کامل دوطرفه با زمان‌سنج |
| هر دو یکی، ولی بی لینک | — | مسیر یا سرور خارج؛ نه نسخه |

## ۸. فرمان بازگشت

**همان شکل ارتقا، با `stage-prod`، اول ایران.** وقتی تونل بالاست، خارج هم (مستقیم یا از راه تونل) در دسترس است. پس **اول هر دو زمان‌سنج را بگذارید**، بعد بگذارید اجرا شوند. افتادن تونل در لحظهٔ بازگشت دیگر مهم نیست.

ایران، در `T2`:

```bash
systemd-run --unit=hs2-r1-back --on-calendar='YYYY-MM-DD HH:MM:SS.000 UTC' \
  --timer-property=AccuracySec=1ms --timer-property=RandomizedDelaySec=0 \
  --setenv=HS2_SAFE_UNIT=hs2 --setenv=HS2_SAFE_PEER_WAIT=120 \
  /root/r1-field/hs2-safe-apply /root/r1-field/stage-prod
```

خارج: همان فرمان با `.300`.

**بازگشت فوری یک سر** (حالت ناهم‌خوان، جدول بخش ۷):

```bash
systemd-run --unit=hs2-r1-fix-$(date +%s) --collect --setenv=HS2_SAFE_UNIT=hs2 \
  --setenv=HS2_SAFE_PEER_WAIT=120 /root/r1-field/hs2-safe-apply /root/r1-field/stage-prod   # یا stage-r1
```

- **sandbox:** بازگشت با فاصلهٔ ۰٫۷ ثانیه، ۱٫۳ ثانیه قطعی داشت (`back-e07`). وقتی خارج خودش برگشت و ایران بی‌درنگ برگردانده شد، تونل ۰٫۳ ثانیه پس از بازگشت ایران آمد (`up-xflap`).
- **کلید بی بازگشت کامل** (مثلاً `HS2_TUNE_QUIET_CC=bbr`): یک drop-in از راه `hs2-safe-apply`، روی هر دو:

  ```bash
  mkdir -p /root/r1-field/stage-cc
  printf '[Service]\nEnvironment=HS2_TUNE_QUIET_CC=bbr\n' > /root/r1-field/stage-cc/dropin.conf
  ```

  سپس `hs2-safe-apply /root/r1-field/stage-cc` با همان زمان‌سنج دوطرفه. این کلید خودش با R1 سازگار است، پس ترتیب حیاتی نیست، ولی هر سر یک بار راه‌اندازی دوباره می‌شود. وارسی: `journalctl -u hs2 | grep 'tuning: HS2_TUNE_QUIET_CC'`.
- برداشتن drop-in: یک `dropin.conf` خالی در stage.

## چند تونل با باینری مشترک

این دستورالعمل هر سرور را با یک تونل فرض می‌کند، و سناریوهای بخش ۱۰ هم همین را سنجیده‌اند. پس آنچه این‌جا می‌آید در sandbox سنجیده نشده است. سروری با چند تونل (`systemctl list-units 'hs2*'`) این قاعده‌ها را دارد:

- **`hs2-safe-apply` هر بار یک سرویس را می‌گیرد:** فقط واحد `HS2_SAFE_UNIT` (پیش‌فرض `hs2`، بقیه `hs2-<نام>`) را راه‌اندازی دوباره و وارسی می‌کند. ولی `/usr/local/bin/hs2` میان همهٔ تونل‌ها مشترک است. تونل دیگر تا راه‌اندازی دوبارهٔ بعدی‌اش با ساخت قدیم در حافظه می‌ماند و بعد، مثلاً پس از کرش، ناگهان ساخت تازه را می‌گیرد.
- **همه با هم:** همهٔ تونل‌های سرور باید در همان پنجره راه‌اندازی دوباره شوند، و دو سر هر تونل طبق قاعدهٔ پایه. هر واحد باید **پس از** جایگزینی باینری راه‌اندازی شود: پیش از خط `applied; restarting` در لاگ، باینری هنوز قدیم است.
- **هر سر نسخهٔ قبلی خودش را نگه دارد:** پیش از پنجره، روی هر سرور جدا:

  ```bash
  install -m 755 /usr/local/bin/hs2 /root/r1-field/stage-prod/hs2 && sha256sum /root/r1-field/stage-prod/hs2
  ```

  بازگشت هر سرور از همین رونوشت خودش است، نه از رونوشت سر دیگر. به پشتیبان `apply-*` تکیه نکنید: اجرایی که پس از نصب باینری به دست اجرای دیگر آغاز شود، باینری تازه را به‌جای نسخهٔ قبلی پشتیبان می‌گیرد.
- **بازگشت یکی، بازگشت همه:** `REVERTING` یک واحد باینری مشترک را برمی‌گرداند ولی فقط همان واحد را راه‌اندازی دوباره می‌کند. پس از آن، بقیهٔ تونل‌های این سرور و سر مقابل هر تونل را هم با بخش ۸ برگردانید.
- **لاگ جدا:** `.last` و `.log` میان اجراها مشترک‌اند. برای هر واحد `HS2_SAFE_LAST` و `HS2_SAFE_LOG` جدا بدهید.

## ۹. پس از پنجره

- `/root/hs2-safe-apply.log` و `.last` هر دو سرور، sha256 باینری، زمان دقیق‌ترین `T` و قطعی دیده‌شده را در گزارش میدان ثبت کنید.
- **`hs2-menu` → «5) Upgrade» را یک‌طرفه نزنید.** این منو باینری `main` را نصب می‌کند، که حالا R1 است، و زدنش فقط روی یک سر تونل را قطع می‌کند (هشدار آغاز همین سند). ایرادی ندارد که `/usr/local/sbin/hs2-safe-apply` کهنهٔ خارج بماند؛ این دستورالعمل از `/root/r1-field/` استفاده می‌کند.
- **پایان سنجش:** خارج را آزاد کنید (`thaw` و قفل، بخش ۴).

## ۱۰. آنچه درون sandbox سنجیده شد

- **ابزار:** `review/tools/fieldug.sh`، `fakesd.sh` و `runall.sh`، بر پایهٔ ابزار ارتقای تیم قرمز ۲. هر سناریو درون `hs2-src/install/tests/sandbox.sh` اجرا شد، با `scripts/hs2-safe-apply` همین شاخه.
- **چیدمان:** دو فضای نام مانند میدان (ایران گوش می‌دهد، خارج شماره می‌گیرد)، RTT ‏۷۵ms، ‏۶ لینک (R1: ‏۶ BULK + ‏۴ QUIET)، و کاوشگری که هر ۱۰۰ms یک اتصال تازه تا سرور پژواک پشت خارج باز می‌کند.
- **باینری‌ها:** تولید (`bd7966970bd0…`، همان `/usr/local/bin/hs2` خارج) و R1 (`9856c2f91654…`).
- **قطعی:** از نخستین کاوش ناموفق تا نخستین کاوش موفق، بلندترین رشتهٔ شکست.
- **میزبان دست نخورد:** در هر اجرا MainPID، NRestarts و وضعیت سرویس تولید بی‌تغییر ماند.

**ستون‌ها:**
- «فاصلهٔ خدمت»: از نخستین راه‌اندازی دوباره تا نخستین اتصال تازه‌ای که پس از هر دو پاسخ گرفت (`tools/gap.py`، `C-ops/sandbox/gaps.txt`).
- «قطعی کاوشگر»: از نخستین کاوش ناموفق تا نخستین کاوش موفقی که پس از آخرین `hs2-safe-apply` آغاز شد.

| سناریو | ترتیب | فاصلهٔ واقعی دو راه‌اندازی | فاصلهٔ خدمت | قطعی کاوشگر | نتیجهٔ `hs2-safe-apply` (ایران، خارج) |
|---|---|---|---|---|---|
| `up-both` | هر دو با هم | خارج +0.03 s | 0.88 s | 0.3 s | OK، OK |
| `up-e03` | ایران، خارج 0.3 s بعد | +0.29 s | 1.15 s | 1.2 s | OK، OK |
| `up-e07` | ایران، خارج 0.7 s بعد | +0.70 s | 1.22 s | 1.3 s | OK، OK |
| `up-x03` | خارج، ایران 0.3 s بعد | +0.30 s | 0.93 s | 0.5 s | OK، OK |
| `up-x06` | خارج، ایران 0.6 s بعد | +0.57 s | 1.50 s | 0.8 s | OK، OK |
| `up-x10` | خارج، ایران 1.0 s بعد | +1.05 s | **9.28 s** | 9.3 s | OK، OK (خارج: «answered as a plain website … dialing it again in 8s») |
| `up-x20` | خارج، ایران 2.0 s بعد | +2.06 s | **8.27 s** | 8.3 s | OK، OK |
| `up-xflap` | ایران، خارج 0.7 s بعد؛ R1 خارج یک بار «می‌افتد» (`NRestarts` = 1) | +0.68 s | 1.27 s؛ پس از بازگشت خارج در ثانیهٔ ۶۱، ایران هم برگردانده شد و تونل 0.3 s بعد آمد | — | OK، REVERTED `hs2-not-up`؛ ایران دوباره OK روی تولید |
| `up-xflapn` | همان، **بی** قاعدهٔ بخش ۷ | +0.26 s | 1.16 s، سپس **قطع تا پایان** (بلندترین رشتهٔ شکست 126.7 s، تا پایان اجرا) | — | OK، REVERTED `hs2-not-up` |
| `up-erefuse` | `hs2-safe-apply` ایران رد می‌کند (پیکربندی stage خطا دارد)؛ خارج تنها | — | 122.9 s | 122.7 s | REFUSED `config-check`، REVERTED `tunnel-not-back-in-120s` |
| `back-e07` | بازگشت R1 ← تولید، ایران اول، 0.7 s | +0.66 s | 1.21 s | 1.3 s | OK، OK |

**زمان‌سنج systemd روی خارج:** سه زمان‌سنج گذرای بی‌خطر (فقط نوشتن زمان) با `AccuracySec=1ms` ‏21، 32 و 27 میلی‌ثانیه پس از ثانیهٔ تعیین‌شده اجرا شدند. یکی با کسر ثانیه (`.4`) هم 18 میلی‌ثانیه پس از آن.
