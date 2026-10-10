# نصب‌کننده و استقرار

> دامنه: `install.sh` در ریشهٔ مخزن (۴۲۵۴ خط) و همهٔ `hs2-src/install/*` (`install.sh`، `e2e/`، `tests/`).
> وضعیت مخزن هنگام بررسی: شاخهٔ main، ثبت `0812bc9`.
> دو نسخهٔ `install.sh` (ریشه و `hs2-src/install/install.sh`) بایت‌به‌بایت یکسان‌اند (`cmp` بدون اختلاف؛ آزمون `release_files_test.sh` هم همین را تضمین می‌کند)، پس هر شمارهٔ خط `install.sh:N` در این سند برای هر دو درست است.
> هش‌های منتشرشده با فایل‌ها می‌خوانند: `hs2-linux-amd64.sha256` = `e27c711a…8d66` و `install.sh.sha256` = `40305f05…6cce` (بررسی‌شده با `sha256sum`).
> آزمون‌های سبک پوسته‌ای همه اجرا شدند و سبز بودند (فهرست در بخش ۱۰)؛ `tm_edit_test.sh` یک بار در مورد «vim واقعی :wq» قرمز شد و در اجرای دوم سبز (ناپایدار). آزمون‌های نیازمند `ip netns` در این محیط قابل اجرا نبودند (فرمان `ip` نصب نیست).

---

## ۱. نقش و جایگاه در کل سیستم

`install.sh` یک اسکریپت bash تک‌فایلی است که سه کار می‌کند:

1. **جادوگر راه‌اندازی** برای دو نقش: «خارج» (سرور پنل/خروجی) و «ایران» (لبه، جایی که کاربر وصل می‌شود). در هر جهت (direct/reverse) یکی از دو طرف «شنونده» است و **لینک راه‌اندازی** `hs2://…` می‌سازد و طرف دیگر «شماره‌گیر» است و لینک را می‌چسباند (`install.sh:1689-1692`).
2. **مدیر تونل** (`hs2-menu` → 3): فهرست، شروع/توقف/بازراه‌اندازی، ویرایش پیکربندی با اعتبارسنجی و بازگشت خودکار، لاگ زنده، پایش زندهٔ الگوی لینک‌ها، تنظیم هسته، درگاه‌ها، عیب‌یابی، گواهی، ارتقای همین تونل، حذف (`install.sh:3758-3796`).
3. **عملیات چرخهٔ عمر**: ارتقا (دانلود باینری + sha256)، پشتیبان/بازگردانی، حذف کامل، پاک‌سازی (`install.sh:4194-4210`).

اصل طراحی: «یک باینری مشترک، چند سرویس». باینری `/usr/local/bin/hs2` بین همهٔ تونل‌ها مشترک است و هر تونل یک سرویس systemd جدا دارد: `hs2` (پیکربندی `/etc/hs2/config.json`) و `hs2-<name>` (پیکربندی `/etc/hs2/hs2-<name>.json`) (`install.sh:28-35`، `install.sh:462`).

**تقسیم کار نصب‌کننده و باینری** (مهم برای جلوگیری از دوباره‌کاری):

| کار | چه کسی | کجا |
|---|---|---|
| نصب بسته‌ها (`iproute2 iptables curl ca-certificates`، سپس `nftables iputils-ping`) | install.sh | `install.sh:278-292` |
| `modprobe tun` و `modprobe tcp_bbr` و پایدارکردن `tcp_bbr` در `/etc/modules-load.d/hs2.conf` | install.sh | `install.sh:289`، `install.sh:299-307`، `install.sh:4092-4093` |
| حذف فایل قدیمی `/etc/sysctl.d/99-hs2.conf` | install.sh | `install.sh:302-305`، `install.sh:4088-4091` |
| همهٔ sysctlها (بافرها، backlog، somaxconn، cc، qdisc، `tcp_tw_reuse=1`، `rp_filter=2` و …) هنگام **هر** شروع سرویس، متناسب با RAM/هسته | باینری (`hs2 run`) | `hs2-src/cmd/hs2/main.go:346-357`، `hs2-src/tune/tune.go:339-370` |
| ساخت رابط TUN، آدرس، MTU، `txqueuelen 2000`، مسیر `/32` به همتا | باینری | `hs2-src/tun/tun_linux.go:122-130` |
| قاعدهٔ nft/iptables برای بلعیدن پاسخ‌های ping خود هسته در icmp | باینری | `hs2-src/encap/echoguard_linux.go:31-33,118-144` |
| پاک‌کردن قواعد icmp یتیم | باینری (`hs2 cleanup`) که نصب‌کننده صدا می‌زند | `hs2-src/cmd/hs2/cleanup.go:20-32`، `install.sh:2406-2413` |
| سقف استخر لینک خودکار (`max_links: 0`) | باینری در هر شروع؛ نصب‌کننده فقط ۰ می‌نویسد | `hs2-src/cmd/hs2/main.go:711`، `install.sh:1778-1790` |
| بارگذاری دوبارهٔ گواهی (SIGHUP و پایش دقیقه‌ای فایل) | باینری | `hs2-src/cmd/hs2/main.go:387-401`، `hs2-src/cmd/hs2/cert.go:118-119` |
| اعتبارسنجی معنایی پیکربندی | باینری (`hs2 check`) که نصب‌کننده قبل از نصب هر پیکربندی صدا می‌زند | `install.sh:862-881`، `hs2-src/cmd/hs2/check.go:31-55` |
| تغییر کلیدهای پیکربندی از منو | باینری (`hs2 config set`، `hs2 ports`) | `install.sh:3170-3173`، `install.sh:3364-3369` |
| فایروال (باز کردن درگاه تونل/کاربر)، NAT، `ip_forward`، MASQUERADE | **هیچ‌کدام** — نه نصب‌کننده و نه باینری (جست‌وجو در کل کد Go فقط قواعد icmp را یافت) | — |

---

## ۲. اجزای اصلی (توابع کلیدی با path:line)

### ۲-۱. پایه و ایمنی اجرا

| جزء | محل | کار |
|---|---|---|
| `set -euo pipefail` و `umask 077` | `install.sh:15`، `install.sh:21` | هر خطای فرمان کشنده است؛ هر فایل/پوشه پیش‌فرض فقط‌ریشه |
| `info/ok/warn/err/die/bail/hr` | `install.sh:67-75` | همه به stderr تا `$(…)` فقط مقدار واقعی بگیرد؛ `die` و `bail` پرچم `HS2_DIED=1` می‌گذارند |
| بررسی ریشه | `install.sh:77` | `Please run as root.` |
| `on_exit` (تله EXIT) | `install.sh:95-130` | پاک‌کردن پوشهٔ ویرایش؛ گزارش توقف ناخواسته؛ بالا آوردن دوبارهٔ هر واحد در `HS2_STOPPED_UNITS` و بازگرداندن پیکربندی پنهان‌شده فقط برای واحدهای `HS2_STASHED_UNITS` |
| جاروی آغازین | `install.sh:137` و `install.sh:147-163` | حذف `*.pre-replace` یتیم و پوشه‌های `.hs2-edit.<pid>.<rand>` صاحب‌مرده (با مقایسهٔ زمان شروع PID و mtime، ۲ ثانیه رواداری) |

### ۲-۲. شبکه و انتخاب IP

| تابع | محل | کار |
|---|---|---|
| `port_free` / `udp_port_free` | `install.sh:166-167` | با `ss -Hltn` / `ss -Hlun` |
| `local_ips` | `install.sh:183-195` | همهٔ IPv4ها جز `lo` و رابط‌های tun تونل‌های hs2؛ خروجی یک‌باره چاپ می‌شود (رفع SIGPIPE) |
| `ask_bind_ip` → `BINDADDR` | `install.sh:207-226` | IP شنود؛ تک‌IP = `0.0.0.0`؛ چند IP: پیش‌فرض PUBIP اگر محلی باشد |
| `ask_egress_ip` → `EGRESSIP` (`bind_local_ip`) | `install.sh:231-245` | IP مبدأ شماره‌گیری؛ خالی = خودکار |
| `ask_user_ip` → `USERIP` (`user_listen_ip`) | `install.sh:254-276` | IP درگاه‌های کاربر؛ روی سرور چند IP پیش‌فرض PUBIP؛ `all` = همه (IPv4 و IPv6) |

### ۲-۳. باینری و منو

| تابع | محل | کار |
|---|---|---|
| `install_prereqs` | `install.sh:278-292` | apt، `modprobe tun`، `tune_kernel` |
| `tune_kernel` | `install.sh:299-307` | فقط BBR و حذف فایل sysctl ایستا |
| `hs2_is_v3` | `install.sh:329` | `hs2 version` باید با `hs2 v3([^0-9]|$)` بخواند |
| `bin_build` | `install.sh:339-344` | استخراج `[build …]` برای بنر منو |
| `verify_download` | `install.sh:346-357` | خروجی ۰ = مطابق، ۱ = ناهمخوان، ۲ = هش در دسترس نیست |
| `install_binary [force]` | `install.sh:359-458` | منطق «نگه‌داشتن باینری موجود»، دانلود، تأیید، پشتیبان محلی، نصب اتمی |
| `install_self` | `install.sh:3819-3843` | کپی اسکریپت جاری به `/usr/local/bin/hs2-menu`؛ اگر از شبکه گرفته شود با `install.sh.sha256` تأیید می‌شود |

### ۲-۴. تونل به‌عنوان سرویس

| تابع | محل | کار |
|---|---|---|
| `unit_cfg` / `use_unit` | `install.sh:462-469` | نگاشت نام واحد ↔ پیکربندی؛ واحد موجود همان `-c` در `ExecStart` خودش را نگه می‌دارد |
| `norm_unit` | `install.sh:474-482` | نام معتبر: حروف کوچک، `[a-z0-9-]`، حداکثر ۲۰، نه `menu`؛ خروجی `hs2` یا `hs2-<n>` |
| `ask_service_name` | `install.sh:516-539` | طرف سازندهٔ لینک نام می‌دهد؛ نام موجود = پرسش جایگزینی |
| `adopt_service_name` | `install.sh:545-578` | طرف چسباننده نام را از لینک می‌گیرد؛ اگر همان سرور باشد پیش‌فرض «جایگزینی» وگرنه «نام آزاد» (`hs2-x-2`) |
| `stop_for_replace` | `install.sh:584-598` | فقط هنگام جایگزینی: پنهان‌کردن پیکربندی قدیم در `<cfg>.pre-replace` و توقف همان تونل |
| `unit_unmask` | `install.sh:770-781` | واحد ماسک‌شده را با هشدار باز می‌کند |
| `restart_unit` | `install.sh:787-795` | بازراه‌اندازی + `tm_healthy` + **PID باید عوض شده باشد** |
| `write_service` | `install.sh:797-851` | نوشتن فایل unit (متن کامل در بخش ۷) |
| `write_cfg_checked` | `install.sh:862-881` | stdin → `<cfg>.new.$$` (umask 177) → `hs2 check` → `mv` اتمی |
| `start_service` | `install.sh:883-916` | enable + `restart_unit`؛ برای طرف چسباننده `verify_tunnel`؛ حذف stash |

### ۲-۵. زیرشبکه و رابط tun

| تابع | محل | کار |
|---|---|---|
| `set_tun_subnet` | `install.sh:604-610` | ایران = base+1، خارج = base+2، هر دو `/30` |
| `block_of` / `valid_block` | `install.sh:616-635` | پایهٔ `/30` و اعتبار درون `10.77.0.0/16` (بدون صفر پیشرو، آخرین اکتت مضرب ۴ و ≤۲۵۲) |
| `used_blocks` | `install.sh:640-653` | زیرشبکه‌های تونل‌های دیگر و هر رابطی با آدرس `10.77.*` |
| `pick_subnet` | `install.sh:660-676` | تصادفی، تا ۵۰۰ تلاش، `10.77.0.0` کنار گذاشته؛ هنگام جایگزینی همان قبلی |
| `ask_subnet` | `install.sh:685-697` | امکان تعیین دستی پایه (U5)؛ Enter = تصادفی |
| `check_link_subnet` | `install.sh:701-720` | زیرشبکهٔ لینک در طرف چسباننده باید آزاد باشد وگرنه `die` بدون هیچ تغییر |
| `cfg_tun_iface` | `install.sh:726-729` | برای `mtcp` خالی (رابط tun ندارد) |
| `free_iface` / `iface_taken` | `install.sh:743-762` | اولین آزاد از `hs0، hs1، …`؛ هنگام جایگزینی همان قبلی |

### ۲-۶. جادوگرها

| تابع | محل |
|---|---|
| `setup_kharej` | `install.sh:1822-1843` |
| `kharej_listener` (direct) | `install.sh:1847-1961` |
| `kharej_dialer` (reverse) | `install.sh:1965-2055` |
| `setup_iran` | `install.sh:2058-2079` |
| `iran_dialer` (direct) | `install.sh:2082-2195` |
| `iran_listener` (reverse) | `install.sh:2200-2348` |
| `ask_transport` | `install.sh:1387-1403` |
| `ask_tun_encap` | `install.sh:1409-1429` |
| `ask_tun_tls_mode` / `tun_tls_carrier` | `install.sh:1437-1450` |
| `ask_direction` | `install.sh:1680-1687` |
| `show_link` / `parse_link` | `install.sh:1694-1741` |
| `use_auto_link_ceiling` / `link_pool_note` | `install.sh:1778-1820` |
| `tunnel_summary` | `install.sh:2351-2361` |

### ۲-۷. گواهی

`get_cert` (`install.sh:1361-1383`)، `cert_standalone` (`install.sh:1083-1100`)، `cert_dns01` (`install.sh:1105-1121`)، `cert_existing` (`install.sh:1334-1355`)، `configure_renewal` (`install.sh:1137-1168`)، توابع وضعیت فقط‌خواندنی `cert_lineage … cert_method_label` (`install.sh:1181-1330`)، و در مدیر تونل `tm_cert_line` (`install.sh:3633-3664`) و `tm_cert` (`install.sh:3673-3756`).

### ۲-۸. مدیر تونل

`tm_list` (`install.sh:2693-2723`)، `tm_details` (`install.sh:2766-2803`)، `tm_monitor` (`install.sh:2807-2898`)، `tm_edit` (`install.sh:3071-3150`)، `tm_apply_restart` (`install.sh:3154-3166`)، `tm_tune` (`install.sh:3179-3220`)، `tm_tune_links` (`install.sh:3242-3356`)، `tm_tune_manual` (`install.sh:3549-3582`)، `tm_ports` (`install.sh:3403-3545`)، `tm_delete` (`install.sh:3587-3604`)، `tm_doctor` (`install.sh:3612-3623`)، `tunnel_manager` (`install.sh:3798-3815`).

### ۲-۹. چرخهٔ عمر

`backup` (`install.sh:3858-3902`)، `prune_backups` (`install.sh:3908-3922`)، `restore` (`install.sh:3927-4003`)، `migrate_config` (`install.sh:4012-4110`)، `auto_backup` (`install.sh:4113-4117`)، `upgrade` (`install.sh:4124-4191`)، `remove_tunnel` (`install.sh:2368-2400`)، `uninstall` (`install.sh:2419-2451`)، `kill_this_tunnel` (`install.sh:2455-2466`)، `status` (`install.sh:2469-2488`).

**هیچ goroutine یا فرایند پس‌زمینه‌ای در نصب‌کننده نیست**؛ تنها حلقه‌های زمان‌دار، `verify_tunnel` و `tm_monitor` و انتظار `tm_healthy` هستند (بخش ۵).

---

## ۳. جریان داده و کنترل، گام‌به‌گام

### ۳-۱. منوی اصلی و ورودی‌های خط فرمان

- بدون آرگومان یا `menu` → `main_menu` (`install.sh:4213-4253`): `1` خارج، `2` ایران، `3` مدیر تونل، `4` وضعیت، `5` ارتقا، `6` پشتیبان، `7` بازگردانی، `8` حذف همه، `0` خروج. بنر شامل `hs2 v3` و در صورت وجود `[build …]` باینری نصب‌شده است (`install.sh:4217-4225`).
- آرگومان‌ها (`install.sh:4194-4210`): `manage`، `upgrade [tunnel]`، `backup`، `restore [file]`، `status`، `uninstall`، `cleanup`، `version`. ناشناخته → پیام راهنما و خروج ۲ با `HS2_DIED=1`.

### ۳-۲. منوهای انتخاب حامل (نقشهٔ کامل به carrier)

`ask_transport` (`install.sh:1387-1403`) پیش‌فرض `[1]`:

| گزینه | TRANSPORT | ادامه | CARRIER نهایی |
|---|---|---|---|
| 1 auto «(recommended)» | `auto` | — | `auto` (`install.sh:171-177`) |
| 2 udp | `udp` | — | `udp` |
| 3 tcp | `tcp` | زیرمنوی «TLS mode: 1) mtcp 2) l3mtcp 3) tls» (پیش‌فرض 1) | `mtcp` / `l3mtcp` / `tls` (`install.sh:1864-1866`، `install.sh:2225-2227`) |
| 4 tun | `tun` | `ask_tun_encap` (پیش‌فرض 1=udp) | udp/icmp/gre/ipip/ipx → `dgtun` با `encap`؛ گزینهٔ 6=tcp → `ask_tun_tls_mode` (پیش‌فرض 1) → `l3mtcp` یا `tls` (`install.sh:1409-1446`) |

حامل‌های `reality` و `noise` در هیچ منویی نیستند؛ نصب‌کننده آن‌ها را فقط در برچسب‌ها می‌شناسد (`install.sh:2655-2661`، `install.sh:3256`).

### ۳-۳. **مسیر l3mtcp (تونل اصلی کاربر)** — دو راه ورود متفاوت

**راه الف: Transport 4 (tun) → 6 (tcp) → TLS mode 1 (mtcp + tun)** — راه «رسمی».
متن منو: «mtcp + tun — the mtcp multi-link pool (self-sizing TLS links) + a tun interface (recommended, fastest)» و یادداشت مهم: «The tun here is a side channel for ping and light traffic, not for bulk — for bulk over a routed tun choose tun → udp or icmp.» (`install.sh:1440-1443`).

direct، روی خارج (`kharej_listener`، `install.sh:1883-1908`):
1. `ask_subnet` (`install.sh:1848`) و `TUNIF=$(free_iface)`؛ `ask_tunnel_port` (پیش‌فرض 2096، `install.sh:1632`)؛ `ask_bind_ip`.
2. کلید مشترک `openssl rand -hex 32` و بذر cover `openssl rand -hex 16` (`install.sh:1851-1855`).
3. `port_free TPORT`؛ `ask_kharej_targets` (پنل پیش‌فرض `127.0.0.1:8443` و `port_map` اختیاری)؛ `ask_domain`؛ `ask_tun_params` (نام رابط، پیش‌فرض اولین آزاد)؛ `ask_tun_mtu` (پیش‌فرض **1320**، بازهٔ پذیرش 68..65535)؛ `tun_tls_carrier`؛ «Also forward UDP on the user ports? [y/N]».
4. `get_cert` (خارج در direct سرور TLS است).
5. `write_cfg_checked` با قالب (بخش ۷-۳)، `chmod 600`، `write_service kharej`، `start_service kharej` (بدون تأیید همتا).
6. `show_link` با `MTU=$TUNMTU`، `ENCAP=tcp`، `TRANSPORT=tun`، `CARRIER=l3mtcp` (`install.sh:1947-1949`).

direct، روی ایران (`iran_dialer`، `install.sh:2113-2144`):
1. `VERIFY_PEER=1`؛ `parse_link`؛ اگر لینک reverse باشد `die`؛ `adopt_service_name`؛ `check_link_subnet`؛ `free_iface`؛ `ask_egress_ip`.
2. اگر `MTU` در لینک نباشد: `die "this link has no MTU field — regenerate it on the kharej with the new installer."` (`install.sh:2118`).
3. `tun_tls_carrier` (هر چیزی جز `tls` → `l3mtcp`)؛ `ask_tun_params`؛ `ask_user_ip`؛ درگاه‌های کاربر (Enter = هیچ = «pure routed tun»)؛ هر درگاه با `valid_uint` و `port_free` (فقط TCP).
4. پیکربندی با `"mode": "dial"`، `max_links: 0` (خودکار)، `min_links: 2`، `per_link: 8`؛ `start_service iran` → `verify_tunnel` تا ۶۰ ثانیه.

reverse: ایران شنونده و سازندهٔ لینک است (`iran_listener`، `install.sh:2250-2289`) و گواهی روی ایران گرفته می‌شود؛ خارج شماره‌گیر است (`kharej_dialer`، `install.sh:1990-2012`) و `sni` و `bind_local_ip` و `min/max/per_link` می‌نویسد. پرسش UDP در reverse فقط وقتی درگاه کاربر داده شود پرسیده می‌شود (`install.sh:2265-2269`).

**راه ب: Transport 3 (tcp) → TLS mode 2 (l3mtcp)** — همان حامل، ولی:
- MTU پرسیده نمی‌شود و **ثابت 1380** نوشته می‌شود (`install.sh:1875`، `install.sh:2103`، `install.sh:1980`، `install.sh:2239`).
- نام رابط پرسیده نمی‌شود (`free_iface` خودکار، `install.sh:1848`، `install.sh:2086`، `install.sh:2201`).
- در لینک `TRANSPORT=tcp` و فیلد MTU خالی است؛ طرف چسباننده شاخهٔ tcp را اجرا می‌کند که درگاه کاربر را **اجباری** می‌کند (`install.sh:2094`، `install.sh:2217`).

نتیجه: دو تونل l3mtcp که با دو راه ساخته شده‌اند MTU پیش‌فرض متفاوت دارند (1320 در برابر 1380).

### ۳-۴. مسیرهای دیگر به‌طور خلاصه

- **tcp/mtcp**: مثل راه ب با `CARRIER=mtcp`؛ رابط tun در پیکربندی نوشته می‌شود ولی ساخته نمی‌شود (`cfg_tun_iface` خالی، `install.sh:726-729`).
- **tun روی datagram (dgtun)**: بدون دامنه و گواهی؛ MTU **ثابت 1280** با پیام توضیحی (`install.sh:1918-1919`)؛ برای icmp/gre/ipip/ipx درگاه تونل پرسیده نمی‌شود و `addr` فقط IP است (`install.sh:1626-1636`)؛ عدد پروتکل ipx (پیش‌فرض 253) در لینک می‌رود و فقط اگر غیرپیش‌فرض باشد در پیکربندی نوشته می‌شود (`install.sh:1641-1645`).
- **udp/auto**: تونل IP صرف بدون درگاه کاربر و بدون `expose`؛ هشدار صریح که کاربر بدون مسیریابی دستی به پنل نمی‌رسد (`install.sh:2190`، `install.sh:2336`). auto هم TCP و هم UDP همان درگاه را آزاد می‌خواهد (`install.sh:1933`).

### ۳-۵. انتهای راه‌اندازی

`setup_*` در آغاز: `auto_backup` → `install_prereqs` → `install_binary` (بدون force) → `use_auto_link_ceiling` (`install.sh:1824-1827`، `install.sh:2060-2063`). در پایان `link_pool_note` (`install.sh:1842`، `install.sh:2078`) و `tunnel_summary`.
`link_pool_note` فقط برای `mtcp|l3mtcp|dgtun` حرف می‌زند؛ اگر `hs2 recommend-links` بیش از ۶۴ بگوید یادداشت «دیده‌شدن الگوی پرشمار» می‌دهد (`install.sh:1816-1819`)؛ برای icmp سقف ۸ را می‌گوید.

### ۳-۶. ارتقا (`upgrade [tunnel]`، `install.sh:4124-4191`)

1. فهرست واحدها؛ با نام فقط همان. چاپ فهرست بازراه‌اندازی؛ اگر جزئی باشد هشدار «باینری مشترک عوض می‌شود».
2. تأیید `Proceed? [y/N]` (یا `HS2_YES=1`)؛ بی‌tty = لغو.
3. `auto_backup` → `install_prereqs` → `install_binary force`.
4. برای هر واحد: `use_unit` → `migrate_config` → `write_service <role>` (role از `"mode": "dial"`) → `enable` → `restart_unit` → `verify_tunnel` (فقط هشدار).
5. `kernel_cleanup`؛ اگر واحدی بالا نیامد `bail`.

### ۳-۷. دانلود و تأیید باینری (`install_binary`، `install.sh:359-458`)

1. اگر باینری v3 سالم هست و `force` نیست: پیش‌فرض **نگه‌داشتن** (U1)؛ پرسش «Update the shared binary from GitHub now? [y/N]» (بی‌tty = نگه‌داشتن، `HS2_YES=1` = به‌روزرسانی).
2. `curl -fL --connect-timeout 10 --retry 2 "$REPO_RAW/hs2-linux-amd64"`.
3. `verify_download`؛ اگر ۱ یا ۲: یک بار دیگر با `?v=<epoch>` (عبور از کش CDN) هر دو فایل؛ اگر باز ۲ و `HS2_ALLOW_UNVERIFIED=1` → نصب تأییدنشده با هشدار؛ وگرنه `die` (fail-closed).
4. اگر GitHub در دسترس نبود: `hs2-linux-amd64` کنار اسکریپت یا در پوشهٔ جاری (**بدون sha256**، فقط `hs2_is_v3`)؛ وگرنه همان باینری نصب‌شده؛ وگرنه `die` با راهنمای کپی از خارج.
5. `install -m755` به `$BIN.new.$$` و `mv -f` اتمی؛ تونل‌های در حال اجرا روی inode قدیم می‌مانند تا بازراه‌اندازی خودشان (`install.sh:443-451`).
6. چاپ sha256 کامل برای مقایسهٔ دو سرور؛ `install_self`.

### ۳-۸. پشتیبان و بازگردانی

**پشتیبان** (`install.sh:3858-3902`): برای هر تونل فایل unit، پیکربندی و `.prev`، گواهی (اگر زیر `/etc/letsencrypt/live/<d>/` باشد کل `live/<d>` و `archive/<d>` و `renewal/<d>.conf`، وگرنه خود فایل‌ها)، باینری و `/etc/sysctl.d/99-hs2.conf` اگر باشد؛ به‌علاوهٔ `hs2-backup-info.txt`. مقصد: `/root/hs2-backups/hs2-<hostname -s>-YYYYmmdd-HHMMSS.tar.gz` با mode 600 و پوشهٔ 700؛ سپس `prune_backups` (نگه‌داشتن ۱۰ تای جدیدتر).

**بازگردانی** (`install.sh:3927-4003`):
1. بی‌آرگومان: جدیدترین با امکان انتخاب.
2. واحدهای داخل پشتیبان از مسیر `systemd/system/hs2*.service` (پشتیبان خیلی قدیمی: `etc/hs2/config.json` → `hs2`).
3. تونل‌های بیرون از پشتیبان دست‌نخورده می‌مانند.
4. توقف واحدهای پشتیبان (ثبت در `HS2_STOPPED_UNITS`) و حذف رابط tun آن‌ها؛ `unit_unmask`.
5. **ضمانت «گسست تمیز»**: اگر تونل دیگری خارج از پشتیبان هست و باینری پشتیبان با نصب‌شده فرق دارد، باینری استخراج نمی‌شود (`install.sh:3970-3988`).
6. `tar -xzf … -C /`، `daemon-reload`، اگر `99-hs2.conf` آمد `sysctl -p`، برای پشتیبان بدون unit `write_service` بر اساس نقش، `enable` و `restart_unit` هر واحد.

### ۳-۹. حذف یک تونل و حذف همه

`tm_delete` (`install.sh:3587-3604`): تایپ نام سرویس → `auto_backup` → `remove_tunnel`.
`remove_tunnel` (`install.sh:2368-2400`): `disable --now` → `kill_this_tunnel TERM` و پس از ۱ ثانیه `KILL` (فقط فرایندی که `-c <cfg>` همین تونل را دارد؛ هرگز `pkill -x hs2`) → حذف رابط tun اگر تونل دیگری همان نام را ندارد → حذف unit، پیکربندی، `.prev`، `.pre-replace`، فایل وضعیت و `.warm` → حذف پوشهٔ drop-in `<u>.service.d` → `daemon-reload` و `reset-failed` → `kernel_cleanup`.
`uninstall` (`install.sh:2419-2451`): تایپ `REMOVE ALL` (U14) → `auto_backup` → `remove_tunnel` برای همه → حذف `/etc/sysctl.d/99-hs2.conf` و `/etc/modules-load.d/hs2.conf` و `/run/hs2/*.warm|*.status.json` و پوشه‌های خالی → پرسش جدا برای حذف `hs2` و `hs2-menu` (پیش‌فرض نگه‌داشتن) → هشدار که پشتیبان‌ها کلید دارند.

### ۳-۱۰. ویرایش پیکربندی (`tm_edit`، `install.sh:3071-3150`)

پوشهٔ خصوصی `/etc/hs2/.hs2-edit.<pid>.<rand>` (700) با کپی 600 → ویرایشگر (`SUDO_EDITOR`→`VISUAL`→`EDITOR` اگر ترمینالی و نصب‌شده، وگرنه nano، وگرنه vi) → بدون تغییر: خروج → خروج غیرصفر ویرایشگر: پرسش → نمایش diff → `tm_validate` (`hs2 check`؛ باینری قدیمی: فقط نحو JSON با python3) → خطا: «باز کردن دوباره / دور ریختن» → ذخیرهٔ `<cfg>.prev` → محاسبهٔ فیلدهای دوطرفهٔ تغییرکرده → `cat tmp > cfg` → `systemctl restart` → `tm_healthy` → در شکست پیشنهاد بازگرداندن `.prev` (پیش‌فرض بله) → هشدار فیلدهای دوطرفه (`shared_key carrier encap sni mtu proto reverse` و «tunnel subnet»، `install.sh:3036-3048`).

### ۳-۱۱. گواهی

`get_cert` (`install.sh:1361-1383`): هشدار DNS اگر A رکورد به این سرور نیست → اگر `/etc/letsencrypt/live/<domain>/fullchain.pem` هست استفادهٔ دوباره → وگرنه منوی ۱) HTTP-01 standalone روی ۸۰ ۲) DNS-01 دستی ۳) گواهی موجود. همهٔ certbotها با `--register-unsafely-without-email` و `--deploy-hook "pkill -HUP -x hs2"`. `configure_renewal` در فایل renewal: `renew_hook = pkill -HUP -x hs2`، `renew_before_expiry = 30 days` در **بالای** فایل، `systemctl enable --now certbot.timer`؛ برای DNS-01 بدون hook هشدار «does NOT renew by itself» به‌جای «Renewal set» (`install.sh:1158-1167`).

---

## ۴. جدول ثابت‌ها، آستانه‌ها و زمان‌سنج‌ها

| نام | مقدار | path:line | معنی |
|---|---|---|---|
| `BIN` | `/usr/local/bin/hs2` | `install.sh:23` | باینری مشترک |
| `REPO_RAW` | `https://raw.githubusercontent.com/aliyaghoobi2323-cloud/hs2-/main` (قابل تغییر با `HS2_REPO_RAW`) | `install.sh:24` | منبع دانلود |
| `CFG_DIR` / `UNIT_DIR` | `/etc/hs2` / `/etc/systemd/system` | `install.sh:25-26` | |
| `MENU_BIN` | `/usr/local/bin/hs2-menu` | `install.sh:2495` | |
| `BACKUP_DIR` | `/root/hs2-backups` | `install.sh:3849` | |
| `BACKUP_KEEP` | `${HS2_KEEP_BACKUPS:-10}`؛ ۰ یا غیرعدد = بی‌حذف | `install.sh:3907-3910` | |
| `TUN_BASE` پیش‌فرض قدیمی | `10.77.0.0` (ایران `.1/30`، خارج `.2/30`) | `install.sh:41-47` | لینک قدیمی |
| فضای زیرشبکه | `10.77.0.0/16`، بلوک‌های `/30` | `install.sh:623-635` | |
| تلاش انتخاب زیرشبکه | ۵۰۰ | `install.sh:667` | سپس `die` |
| `LINK_MIN` | 2 | `install.sh:55` | |
| `LINK_MAX` | 32 در متغیر، ولی `use_auto_link_ceiling` آن را **0** می‌کند | `install.sh:56`، `install.sh:1780` | ۰ = سقف خودکار |
| `LINK_PER` | 8 | `install.sh:57` | |
| `TUN_ENCAP` پیش‌فرض | `udp` | `install.sh:63` | |
| درگاه تونل پیش‌فرض | 2096 | `install.sh:1632` | |
| پنل پیش‌فرض | `127.0.0.1:8443` | `install.sh:1521` | |
| MTU l3mtcp/tls (راه tun) | 1320 (پذیرش 68..65535) | `install.sh:1668-1671` | «matches Backhaul» |
| MTU مسیر tcp (mtcp/l3mtcp/tls) | 1380 ثابت | `install.sh:1875،1980،2103،2239` | |
| MTU dgtun/udp/auto | 1280 ثابت | `install.sh:1918،1925،1939،2027،2044،2164،2182،2311،2329` | |
| پروتکل ipx پیش‌فرض | 253 (0..255) | `install.sh:1604-1607` | |
| طول نام سرویس | ≤ 20 | `install.sh:480` | |
| طول نام رابط | ≤ 15، `[a-zA-Z0-9_-]` | `install.sh:1654-1655` | |
| طول دامنه | ≤ 253 | `install.sh:1485` | |
| `curl` باینری/هش | `--connect-timeout 10 --retry 2` | `install.sh:348،402،416` | |
| `curl` اسکریپت منو | `--connect-timeout 10` | `install.sh:3825،3830` | |
| `HS2_VERIFY_SECS` | 60 | `install.sh:902،4173` | پنجرهٔ اثبات اتصال |
| فاصلهٔ پروب `verify_tunnel` | 2 ثانیه | `install.sh:984` | ~۳۰ پروب در ۶۰ ثانیه |
| `ping` در `tunnel_up` | `-c1` (فهرست: `-c2`)، `-W2` | `install.sh:948-949` | |
| تازگی فایل وضعیت | ≤ 7 ثانیه | `install.sh:2532` | باینری هر ۲ ثانیه می‌نویسد (`hs2-src/cmd/hs2/status.go:29`) |
| کش همتا در فهرست | 15 ثانیه | `install.sh:2686` | |
| `tm_healthy` | خواب 1.5 + 4 ثانیه، PID یکسان و active | `install.sh:2666-2671` | |
| `tm_monitor` | بازنویسی هر 2 ثانیه (`read -t 2` یا `sleep 2`) | `install.sh:2891-2895` | |
| مکث `remove_tunnel` | 1 ثانیه پیش و پس از TERM | `install.sh:2373-2375` | |
| `on_exit` پس از start | 2 ثانیه | `install.sh:124` | |
| رواداری جاروی ویرایش | 2 ثانیه | `install.sh:159` | |
| `Restart=always`, `RestartSec=3` | | `install.sh:840-841` | |
| `TimeoutStopSec=8` | | `install.sh:842` | |
| `LimitNOFILE=1048576` | | `install.sh:845` | |
| `StartLimitIntervalSec=0` (systemd ≥ 230) یا `StartLimitInterval=0` در `[Service]` | | `install.sh:804-809` | هرگز تسلیم نشدن |
| آستانهٔ «تمدید عقب‌افتاده» | `days < rb - 2` | `install.sh:3656` | |
| `rb` پیش‌فرض | صریح، وگرنه یک‌سوم عمر (زیر ۱۰ روز: نصف)، وگرنه 30 | `install.sh:1228-1247` | |
| آستانهٔ هشدار گواهی دستی/خودی | ≤ 30 روز | `install.sh:3645،3648` | |
| `CERT_HOOK` | `pkill -HUP -x hs2` | `install.sh:1128` | |
| پیش‌تنظیم‌های دستی tune | کم: 8MB/2048/1024؛ متوسط: 16MB/8192/4096؛ زیاد: 32MB/16384/8192؛ سفارشی: n MB، 8192، 4096 | `install.sh:3560-3565` | با `profileValues` باینری یکی است (`hs2-src/tune/tune.go:280-289`) |
| بازهٔ مجاز ویرایش استخر | min/per: 1..1024؛ max: 1..1024 یا `auto` | `install.sh:3329-3335` | |
| مرز «یادداشت دیده‌شدن» | سقف > 64 | `install.sh:1816`، `install.sh:3308` | |
| سقف icmp | 8 | `install.sh:1806`؛ `hs2-src/cmd/hs2/main.go:678` | |
| قانون سقف خودکار باینری | ۱ لینک به ازای ۴۸MB RAM، حداکثر ۳۰۰، زیر ۴ هسته ≤۱۲۸، تک‌هسته = پروفایل، هرگز کمتر از ۳۲/۴۸/۶۴ | `hs2-src/tune/tune.go:198-251` | |

---

## ۵. حلقه‌های کنترلی

| حلقه | ورودی | شرط | خروجی | دوره |
|---|---|---|---|---|
| `verify_tunnel` (`install.sh:977-986`) | پیکربندی، ثانیه‌ها | `tunnel_up` موفق یا اتمام مهلت | ۰/۱ | هر ۲ ثانیه تا ۶۰ |
| `tunnel_up` (`install.sh:934-957`) | carrier، iface، peer_ip، فایل وضعیت | برای هر حامل غیر از `mtcp` با رابط: یک پاسخ ping روی رابط؛ برای `mtcp`: فایل وضعیت تازه و `links > 0` | ۰/۱ | یک‌باره |
| `tm_monitor` (`install.sh:2807-2898`) | فایل وضعیت | فشردن Enter | صفحهٔ زنده | ۲ ثانیه |
| `tm_peer_ok` (`install.sh:2681-2691`) | واحد | کش < 15 ثانیه | ۰/۱ | تنبل |
| `tm_healthy` (`install.sh:2666-2671`) | واحد | active و PID ثابت در ۴ ثانیه | ۰/۱ | ۵٫۵ ثانیه |
| جادوگر پرسش‌ها | ورودی tty | مقدار معتبر | تکرار پرسش (اغلب) یا `die` (برخی) | — |
| `pick_subnet` | بلوک‌های استفاده‌شده | بلوک آزاد | `set_tun_subnet` | تا ۵۰۰ |
| systemd | فرایند | خروج | بازراه‌اندازی | ۳ ثانیه، بی‌پایان |
| certbot.timer | — | — | تمدید | دو بار در روز (تایمر خود certbot) |
| پایش گواهی باینری | فایل‌های گواهی | تغییر روی دیسک | بارگذاری دوباره | هر دقیقه (`hs2-src/cmd/hs2/cert.go:118-119`) |

پرسش‌هایی که در خطا **تکرار می‌شوند**: IP شنود/خروج/کاربر، نام سرویس، زیرشبکه، دامنه، پنل، `port_map`، نام رابط، MTU. پرسش‌هایی که در خطا **`die`** می‌کنند: درگاه تونل (`install.sh:1633-1634`)، درگاه‌های کاربر (`install.sh:2096`)، عدد ipx، گزینه‌های نامعتبر منو، گواهی، جهت.

---

## ۶. حالت‌ها، گذارها، خطاها و بازیابی

### ۶-۱. حالت واحد در مدیر (`tm_state`، `install.sh:2549-2567`)

| `ActiveState` | شرط | نمایش |
|---|---|---|
| `active` | — | `● running` |
| `activating` | `NRestarts > 0` | `✗ crashing (restarting every 3s — see the log)` |
| `activating` | `NRestarts = 0` | `◐ starting` |
| `failed` | — | `✗ failed` |
| غیره | — | `○ stopped` |

به‌علاوه برچسب اتصال واقعی: `✓ connected (N links)` یا `✗ NOT connected — nothing comes back from the other server` (`install.sh:962-971`) و در فهرست `✗ no peer` (`install.sh:2711-2713`).

### ۶-۲. شبکهٔ ایمنی

- **بازراه‌اندازی خودکار پس از قطع**: هر واحدی که اسکریپت متوقف کرده (`stop_for_replace`، `restore`) در `on_exit` دوباره بالا می‌آید؛ اگر stash همین اجرا هست اول پیکربندی قدیم برمی‌گردد (`install.sh:104-127`).
- **stash فقط متعلق به همین اجرا**: `.pre-replace`های قدیمی در آغاز پاک می‌شوند (`install.sh:137`) تا هرگز روی پیکربندی جدیدتر نوشته نشوند.
- **پیکربندی اتمی** (`write_cfg_checked`): پیکربندی نامعتبر هرگز نصب نمی‌شود؛ باینری قدیمی بدون `check` → بررسی نحو با python3 → اگر python3 نبود نوشتن ساده.
- **باینری fail-closed** (`install.sh:406-425`).
- **بازگشت پس از ویرایش/تنظیم/درگاه**: همه `.prev` می‌سازند و در صورت بالا نیامدن برمی‌گردانند (`install.sh:3139-3149`، `install.sh:3154-3166`، `install.sh:3336-3355`، `install.sh:3569-3581`). تغییر چنددرگاهی «همه یا هیچ» است (`install.sh:3391-3401`) و اگر `.prev` نوشته نشود تغییری انجام نمی‌شود (`install.sh:3382-3387`).
- **واحد ماسک‌شده**: باز می‌شود؛ بازراه‌اندازی‌ای که PID را عوض نکند شکست است نه «running» (`install.sh:787-795`).
- **راه‌اندازی بدون اتصال همتا**: سرویس نصب و در حال اجرا می‌ماند و خودش تلاش می‌کند؛ پیام تشخیصی بر اساس حامل (`tunnel_down_help`، `install.sh:991-1022`) و «installed and running but is NOT connected yet» به‌جای «ready» (`install.sh:922-928`).
- **شکست شروع** (`start_service`): لاگ ۲۰ خط و `bail`؛ اگر جایگزینی بود `on_exit` پیکربندی قدیم را برمی‌گرداند.

### ۶-۳. خطاهای کشندهٔ مهم (متن دقیق)

- `the downloaded binary could not be verified against its published sha256 (a mismatch, or the hash could not be fetched) — NOT installed. …` (`install.sh:423`)
- `binary is outdated or corrupt (need hs2 v3). Re-download hs2-linux-amd64.` (`install.sh:442`)
- `download failed. On the Iran server, copy hs2-linux-amd64 from the kharej server into $(pwd) and run again.` (`install.sh:438`)
- `the config was rejected by 'hs2 check' (see the error above) — not installed. …` (`install.sh:878`)
- `The link's tunnel subnet $b/30 is already used here by tunnel $by …` و `Run the setup again on the OTHER server … Nothing was changed here.` (`install.sh:715-717`)
- `this link is a REVERSE link; …` / `this link is a DIRECT link; …` (`install.sh:2085`، `install.sh:1969`)
- `this link has no MTU field — regenerate it …` (`install.sh:1994`، `install.sh:2118`)
- `could not find a free tunnel subnet in 10.77.0.0/16` (`install.sh:675`)

---

## ۷. قالب‌ها (لینک، unit، پیکربندی، فایل وضعیت، پشتیبان)

### ۷-۱. لینک راه‌اندازی `hs2://`

`hs2://` + `base64 -w0` از ۱۳ فیلد جداشده با `|` (`install.sh:1694-1702`):

| # | فیلد | توضیح |
|---|---|---|
| 1 | ENDPOINT | `PUBIP:TPORT` یا فقط IP برای icmp/gre/ipip/ipx |
| 2 | DOMAIN | یا `-` |
| 3 | SHARED | کلید ۶۴ هگز (**به‌صورت آشکار در لینک**) |
| 4 | PANEL | پنل خارج در direct؛ در reverse `-` |
| 5 | CARRIER | |
| 6 | UDP | `true`/`false` |
| 7 | TRANSPORT | `auto/udp/tcp/tun` |
| 8 | DIRECTION | `direct/reverse` |
| 9 | MTU | فقط tun؛ برای tun→tcp لازم |
| 10 | ENCAP | فقط tun |
| 11 | PROTO | فقط ipx |
| 12 | UNIT | نام سرویس |
| 13 | TUN_BASE | پایهٔ `/30` |

سازگاری عقب‌رو (`parse_link`، `install.sh:1710-1741`): نبود CARRIER = `mtcp`، نبود TRANSPORT از روی CARRIER، نبود DIRECTION = `direct`، لینک tun قدیمی بی‌ENCAP = `tcp`، نبود نام/زیرشبکه = `hs2` و `10.77.0.0`. PROTO نامعتبر خالی می‌شود و دوباره پرسیده می‌شود؛ MTU غیرعددی یا با صفر پیشرو `die`؛ هر عدد معتبر JSON بی‌تغییر پذیرفته می‌شود. فیلد UDP اعتبارسنجی نمی‌شود و مستقیم به‌صورت مقدار خام JSON نوشته می‌شود (`hs2 check` آن را می‌گیرد).

### ۷-۲. فایل unit

```
[Unit]
Description=hs2 tunnel $UNIT ($role)
Documentation=https://github.com/aliyaghoobi2323-cloud/hs2-
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=0
[Service]
Type=simple
ExecStartPre=-/sbin/modprobe tun
ExecStart=/usr/local/bin/hs2 run -c $CFG
ExecReload=/bin/kill -HUP $MAINPID
Restart=always
RestartSec=3
TimeoutStopSec=8
KillMode=mixed
KillSignal=SIGTERM
LimitNOFILE=1048576
[Install]
WantedBy=multi-user.target
```
(`install.sh:824-849`). عمداً بدون `User=`، `CapabilityBoundingSet=` و `NoNewPrivileges=`: ریشه با همهٔ قابلیت‌ها، چون tune به `CAP_SYS_ADMIN` و کپسوله‌های خام به `CAP_NET_RAW` نیاز دارند (`install.sh:816-823`). **نصب‌کننده هیچ drop-in نمی‌نویسد**؛ فقط هنگام حذف تونل پوشهٔ `<u>.service.d` را پاک می‌کند (`install.sh:2391-2396`). README برای متغیرهای محیطی (مثل `HS2_ICMP_SUPPRESS`) استفاده از `systemctl edit` را پیشنهاد می‌کند.

### ۷-۳. قالب‌های پیکربندی l3mtcp (کلیدها دقیقاً همان‌طور که نوشته می‌شوند)

خارج، direct (شنونده، `install.sh:1897-1907`):
```
"mode": "listen", "carrier": "l3mtcp", "reverse": false,
"addr": "$BINDADDR:$TPORT",
"iface": "$TUNIF", "local_cidr": "<base+2>/30", "peer_ip": "<base+1>", "mtu": $TUNMTU,
"backend_addr": "builtin", "cover_seed": "<32hex>", "shared_key": "<64hex>",
"cert_file": "...", "key_file": "...",
"expose": "$PANEL"[, "port_map": "..."]
```
(بدون `min_links/max_links/per_link` — در direct سقف خارج اعمال نمی‌شود.)

ایران، direct (شماره‌گیر، `install.sh:2128-2138`):
```
"mode": "dial", "carrier": "l3mtcp", "reverse": false, "udp": $UDP,
"addr": "$ENDPOINT", "sni": "$DOMAIN",
"iface": "$TUNIF", "local_cidr": "<base+1>/30", "peer_ip": "<base+2>", "mtu": $MTU,
"shared_key": "...", "forward_ports": "$PORTS", "peer_panel": "$PANEL", "user_listen_ip": "$USERIP",
"min_links": 2, "max_links": 0, "per_link": 8, "bind_local_ip": "$EGRESSIP"
```

ایران، reverse (شنونده، `install.sh:2272-2283`): `"mode": "dial", "reverse": true, "udp"`، `"addr": "$BINDADDR:$TPORT"`، `backend_addr/cover_seed/cert_file/key_file`، `forward_ports`، `user_listen_ip`، `min/max/per_link`.
خارج، reverse (شماره‌گیر، `install.sh:1998-2007`): `"mode": "listen", "reverse": true`، `"addr": "$ENDPOINT", "sni"`، `expose[,port_map]`، `min/max/per_link`، `bind_local_ip`.

قاعدهٔ نقش: `mode=dial` یعنی همیشه ایران؛ `reverse` فقط عوض می‌کند چه کسی وصل می‌شود (`install.sh:2580-2584`).
کلیدهایی که نصب‌کننده **هرگز** نمی‌نویسد: `tuning` (فقط از منوی Tuning)، `drain_idle_sec`، `backend_addr` سفارشی، `reality`/`noise`.

### ۷-۴. فایل وضعیت زنده (خوانده‌شده، نه نوشته‌شده)

مسیر: `/run/hs2/` + مسیر مطلق پیکربندی با `/`→`-` و فاصله→`_` + `.status.json` (`install.sh:2520-2525`، هم‌خوان با `hs2-src/cmd/hs2/status.go:182-191`). فایل `.warm` کنار آن (`hs2-src/cmd/hs2/status.go:195-197`). کلیدهایی که نصب‌کننده با grep می‌خواند: `updated links target counted min max users mbit phase sat serving flowing pressed peak_mbit retiring held_by held_active cap_mbit reason exit_stats refill ceiling_text carrier policed police_confirmed loss_pct max_loss_pct parity_pct fec_at_ceiling fec_recovered fec_lost police_cap_mbit send_refused rx_dropped tun_drops cpu_pct cpu_cores host_cpu_pct host_cores psi_cpu10 host_saturated host_out_discards tun_read sent_pkts recv_pkts tun_written drop_no_carrier drop_queue_full drop_aged` (`install.sh:2608-2648`، `install.sh:2729-2759`، `install.sh:2821-2831`، `install.sh:3231`).

### ۷-۵. بایگانی پشتیبان

مسیرهای نسبی ریشه (`etc/systemd/system/hs2*.service`، `etc/hs2/*.json[.prev]`، `etc/letsencrypt/{live,archive}/<d>`، `etc/letsencrypt/renewal/<d>.conf`، `usr/local/bin/hs2`، `etc/sysctl.d/99-hs2.conf`) به‌علاوهٔ `hs2-backup-info.txt` در ریشهٔ بایگانی (`install.sh:3867-3896`).

---

## ۸. متن پیام‌های مهم و معنی‌شان

| متن | محل | معنی |
|---|---|---|
| `The installer stopped unexpectedly (status $rc) at: $cmd` | `install.sh:101` | خروج ناخواسته زیر `set -e`؛ فرمان مقصر چاپ می‌شود |
| `$u was stopped and is not running — starting it again with its current config…` | `install.sh:122` | شبکهٔ ایمنی |
| `$u: an interrupted re-setup — restoring its previous config before starting it.` | `install.sh:117` | بازگرداندن stash |
| `sha256 matches the published hash.` / `sha256 mismatch: published …, downloaded …` | `install.sh:354-355` | |
| `Verifying once more past the CDN cache…` | `install.sh:414` | تلاش دوم |
| `GitHub unreachable — using local $f (make sure it is the NEW one).` | `install.sh:428` | بدون sha256 |
| `Keeping the installed hs2.` | `install.sh:392` | U1 |
| `Link-pool ceiling: auto (max_links 0) — … right now: $why.` | `install.sh:1783` | |
| `… The installed hs2 binary predates it and runs it as a fixed 32 until hs2 is upgraded …` | `install.sh:1787` | باینری قدیمی |
| `SETUP LINK — copy it to the OTHER server:` | `install.sh:1699` | |
| `Checking that the tunnel really reaches the other server (up to ${secs} s)…` | `install.sh:980` | |
| `Tunnel is UP — the other server answered through it.` | `install.sh:982` | |
| `The tunnel did NOT connect: hs2 is running here, but nothing came back from the other server.` | `install.sh:995` | |
| `$UNIT ($role) is running with the new config.` | `install.sh:890` | |
| `Autostart on boot: ON — it comes back by itself after a reboot or crash.` | `install.sh:891` | |
| `$u was MASKED (by something outside this installer) — unmasking it …` | `install.sh:776` | |
| `Tunnel MTU is fixed at 1280 here — …` | `install.sh:1919` | dgtun |
| `Adaptive link pool updated to 2–32 …` | `install.sh:4030` | مهاجرت |
| `This is a classic tun over multi-link TLS (l3mtcp) — still supported and unchanged.` | `install.sh:4040` | مهاجرت؛ برای l3mtcp هیچ تغییری اعمال نمی‌شود |
| `Cover page is now unique to this install (per-install seed added).` | `install.sh:4081` | |
| `This certificate does NOT renew by itself: DNS-01 needs a new TXT record each time.` | `install.sh:1162` | |
| `Renewal set: 30 days before expiry, hot-reload (no downtime). Timer: certbot.timer.` | `install.sh:1166` | |
| `You changed setting(s) that MUST be the SAME on the OTHER server:` | `install.sh:3058` | |
| `hs2 upgraded. Upgrade the OTHER server(s) too (both sides of a tunnel must match).` | `install.sh:4189` | |
| `$u has not reconnected yet. If the OTHER server still runs the old version, upgrade it too …` | `install.sh:4174-4175` | |
| `This backup's hs2 binary differs from the installed one, and other tunnels here are not in this backup — keeping the installed binary …` | `install.sh:3978-3979` | گسست تمیز |
| `Cleaned up …` | `install.sh:2410` | خروجی `hs2 cleanup` |

---

## ۹. گزینه‌های پیکربندی و متغیرهای محیطی

### ۹-۱. متغیرهای محیطی نصب‌کننده

| متغیر | پیش‌فرض | اثر | محل |
|---|---|---|---|
| `HS2_REPO_RAW` | GitHub raw شاخهٔ main | منبع باینری، هش و اسکریپت؛ برای سنجاق کردن یک ثبت | `install.sh:24`، `install.sh:309-315` |
| `HS2_ALLOW_UNVERIFIED=1` | — | نصب وقتی هیچ هشی منتشر نشده (فقط حالت ۲) | `install.sh:419` |
| `HS2_YES=1` | — | پاسخ «بله» به به‌روزرسانی باینری و «Proceed?» ارتقا | `install.sh:380`، `install.sh:4148` |
| `HS2_KEEP_BACKUPS` | 10 | تعداد پشتیبان؛ ۰ = بی‌حذف | `install.sh:3907` |
| `HS2_VERIFY_SECS` | 60 | پنجرهٔ `verify_tunnel` | `install.sh:902`، `install.sh:4173` |
| `SUDO_EDITOR`/`VISUAL`/`EDITOR` | — | ویرایشگر | `install.sh:2954-2973` |

متغیرهای درونی (نه برای کاربر): `HS2_DIED`، `HS2_STOPPED_UNITS`، `HS2_STASHED_UNITS`، `HS2_EDIT_TMP`، `VERIFY_PEER`، `PEER_UNVERIFIED`، `REPLACING`.

### ۹-۲. متغیرهای محیطی باینری که در آزمون‌های نصب‌کننده به کار می‌روند

`HS2_NO_TUNE=1` (اعمال نکردن sysctl، `hs2-src/cmd/hs2/main.go:349-357`)، `HS2_BIN` و `PROBE_BIN` (باینری آماده برای آزمون‌ها)، `HS2_INSTALL_SH` (آزمون روی نصب‌کنندهٔ دیگر)، `HS2_SKIP_TUNNEL=1`، `KEEP=1`.

### ۹-۳. کلیدهایی که منوها با `hs2 config set` تغییر می‌دهند

`tuning.mode` (`auto|manual|off`)، `tuning.congestion` (پیش‌فرض پیشنهادی `bbr`)، `tuning.qdisc` (پیش‌فرض `fq_codel`)، `tuning.rmem_max`، `tuning.wmem_max`، `tuning.netdev_backlog`، `tuning.somaxconn` (`install.sh:3204-3214`، `install.sh:3572-3576`)، `min_links`، `max_links` (۰ = auto)، `per_link` (`install.sh:3343-3349`). ترتیب نوشتن min/max طوری انتخاب می‌شود که بررسی گام‌به‌گام `min<=max` باینری رد نکند (`install.sh:3337-3349`).
درگاه‌ها با `hs2 ports -c <cfg> add|remove|udp on/off|default <hp|none>` (`install.sh:3440-3535`).

---

## ۱۰. آزمون‌ها: چه چیزی تضمین می‌شود

همه با استخراج توابع از `install.sh` (یا کل فایل تا پیش از `case "${1:-}" in`) و اجرا زیر همان `set -euo pipefail`؛ بسیاری روی pty واقعی (`script -qec`).

| آزمون | تضمین‌ها | نیاز |
|---|---|---|
| `tests/release_files_test.sh` | دو نسخهٔ `install.sh` یکسان؛ `hs2-linux-amd64.sha256` و `install.sh.sha256` با فایل‌ها می‌خوانند | — (اجرا شد: سبز) |
| `tests/helpers_test.sh` | `verify_download` دقیقاً ۰/۱/۲ (هش آشغال = ۲)؛ `kill_this_tunnel` فقط همین پیکربندی را می‌کشد و PID بازیافتی غیر hs2 را نه؛ `prune_backups` ۱۰ تا نگه می‌دارد و `HS2_KEEP_BACKUPS=0` خاموشش می‌کند؛ نام فایل `.warm`؛ `remove_tunnel` فایل `.warm` و drop-in را پاک می‌کند؛ `uninstall` `/run/hs2` را پاک می‌کند و برای حذف برنامه می‌پرسد | go (اختیاری) — سبز |
| `tests/migrate_test.sh` | مهاجرت با یک گواهی certbot بیگانه متوقف نمی‌شود؛ ۸/۱۶/۸ → ۲/۳۲/۸؛ JSON معتبر؛ `renew_hook = pkill -HUP -x hs2`؛ `renew_before_expiry` بالای `[renewalparams]`؛ گواهی پنل دست‌نخورده؛ حذف فایل sysctl ایستا؛ اجرای دوم بی‌اثر؛ ۷ روز → ۳۰ روز | — سبز |
| `tests/multi_ip_test.sh` | `first_public_ip` روی سرور ۶ IP بدون SIGPIPE | — سبز |
| `tests/cover_seed_test.sh` | هر heredoc با `backend_addr: builtin` بذر دارد (۴ تا)؛ بذر از `openssl rand` نه کلید؛ مهاجرت بذر یکتا اضافه می‌کند، idempotent، `backend_addr` سفارشی یا `builtin.example.com` دست‌نخورده | — سبز |
| `tests/link_ceiling_test.sh` | راه‌اندازی جدید `LINK_MAX=0`؛ باینری قدیمی/ناموجود بدون سقوط و بدون نمایش `unknown command`؛ `link_pool_note` درست برای هر نقش و icmp=۸؛ هر دو setup بلافاصله پس از `install_binary` آن را صدا می‌زنند؛ مهاجرت ۳۲ ثابت می‌نویسد؛ صفحهٔ Link pool: نمایش `auto, now N`، Enter نگه‌دارنده، `auto`=۰، ترتیب نوشتن، رد مقدار بد، نمایش خط دقیق `ceiling_text` و رد فایل وضعیت کهنه | script — سبز |
| `tests/tm_tune_links_test.sh` | برای `mtcp/l3mtcp/dgtun`: ایران direct «values that count»، ایران reverse «caps … ITS OWN min/max»، خارج reverse «enforces its min/max as limits»، خارج direct چیزی نمی‌نویسد؛ `tls` «exactly ONE link»؛ `udp/auto/noise/reality` «single session»؛ icmp سقف ۸ | script — سبز |
| `tests/kharej_stats_test.sh` | نمای خارج با `counted: true` کاربران و سرعت و اوج؛ باینری قدیمی «counted on the Iran server»؛ خط `Refill:` | script — سبز |
| `tests/cert_test.sh` | `tm_cert_line` برای DNS-01، hook، standalone (درگاه آزاد/اشغال، pre_hook، پوشهٔ hook، `cli.ini`، فایل نقطه‌دار، پشتیبان `~`، `no-directory-hooks`، `http01_port=8888`)، عقب‌افتاده، `renew_before_expiry=10`، `autorenew=False`، بدون conf، گواهی خودی، منقضی، گواهی ۴۵روزه؛ `cert_domains` بدون glob؛ `configure_renewal`؛ اقدام `c)` از منوی واقعی دقیقاً فرمان درست certbot را اجرا می‌کند | openssl، script، go (اختیاری) — سبز |
| `tests/tm_edit_test.sh` | نبود `trap … RETURN`؛ بازگشت منو پس از ویرایش؛ پاک شدن پوشهٔ ویرایش در همهٔ مسیرها (SIGTERM، `set -e`)؛ فایل‌های جانبی ویرایشگر؛ `:cq`؛ ترتیب انتخاب ویرایشگر؛ عدم گسترش glob؛ جاروی آغازین با PID زنده/مرده/بازیافتی؛ vim واقعی | script، vim — یک بار قرمز (vim :wq) و بار دوم سبز |
| `tests/ports_menu_test.sh` | صفحهٔ Ports در ایران و خارج با باینری واقعی؛ رد درگاه اشغال/بد/تکراری/درگاه تونل؛ همه-یا-هیچ؛ عدم پشتیبان = عدم تغییر؛ حذف آخرین درگاه mtcp رد می‌شود؛ UDP روشن/خاموش؛ اهداف خارج و IPv6؛ پنل `none`؛ `ask_port_map`؛ شش قالب خارج قطعهٔ `port_map` را دارند | script، go — سبز |
| `tests/tun_ports_test.py` | بخش ۱: هر چهار شاخه (خارج/ایران × direct/reverse) برای `l3mtcp` و `tls` و dgtun (udp/ipx/gre) از منوی واقعی؛ پیکربندی‌ها `expose`/`forward_ports`/carrier از لینک/`udp`/`proto`/`addr` درست دارند. بخش ۲: واحد ماسک‌شده باز می‌شود؛ ماسک ماندگار = شکست؛ PID یکسان = شکست. بخش ۳ (ریشه + netns): `hs2 check` تمیز، ping روی tun، `verify_tunnel` بالا، **شمار لینک بدون ping اثبات نیست**، ترافیک واقعی به پنل. بخش ۴: gre مسدود → `verify_tunnel` شکست و نام علت؛ با بازشدن مسیر خودش وصل می‌شود | بخش ۱-۲ اجرا شد: سبز؛ ۳-۴ نیازمند `ip` (اجرا نشد) |
| `tests/multi_tunnel_test.py` | ۱۱ سناریو: نام پیش‌فرض، نام و زیرشبکه در لینک، رابط جدا (`hs1`)، سه تونل هم‌زمان، برخورد نام در هر دو طرف (`hs2-k4-2`)، icmp بدون درگاه و ping عادی سالم، رد زیرشبکهٔ اشغال بی‌تغییر، حذف از مدیر با پشتیبان، جایگزینی با حفظ زیرشبکه/رابط حتی با تغییر حامل، لغو نیمه‌کاره و بازگشت پیکربندی قدیم، restart/stop/start فقط یک تونل، بازیابی از kill، ارتقای همه با گزارش تنها شنوندهٔ بی‌همتا، پشتیبان→حذف→بازگردانی، حذف mtcp رابط tun دیگری را حذف نمی‌کند، `status` اتصال واقعی، `uninstall` همه‌چیز (قاعدهٔ nft هم)، لینک ۱۱فیلدی قدیمی | ریشه + `ip netns` (اجرا نشد)؛ به گفتهٔ CHANGELOG «54 PASS» |
| `tests/dgtun_wildcard_test.sh` | خروجی dgtun با پنل روی `0.0.0.0:<port>` crash نمی‌کند و ترافیک عبور می‌کند | ریشه + `ip` (اجرا نشد) |
| `tests/fakesd.py` | جایگزین واقعی `systemctl`/`journalctl` (سرپرست با Restart، mask، show، reload) برای دو آزمون بالا | — |
| `e2e/run.sh` + `e2e/drv.py` | دو کانتینر Ubuntu 24.04 با systemd واقعی، `curl | bash` از GitHub ساختگی، ~۷۰ رفتار (نصب، مدیر، راه‌اندازی دوباره، ارتقا، چرخهٔ عمر، direct، تطبیقی) | docker — اجرا نشد؛ **احتمالاً کهنه** (بخش ۱۳) |

---

## ۱۱. «از قبل وجود دارد» (برای جلوگیری از دوباره‌کاری)

- چند تونل هم‌زمان با سرویس، پیکربندی، رابط tun و زیرشبکهٔ `/30` جدا؛ نام و زیرشبکه در لینک؛ تشخیص برخورد نام و زیرشبکه در هر دو طرف.
- انتخاب زیرشبکهٔ تصادفی یا دستی (U5) و حفظ زیرشبکه/رابط هنگام جایگزینی.
- جهت direct/reverse برای همهٔ حامل‌ها؛ IP شنود، IP خروج (`bind_local_ip`) و IP درگاه کاربر (`user_listen_ip`) روی سرور چند IP.
- لینک ۱۳فیلدی با سازگاری عقب‌رو تا لینک ۹فیلدی.
- سقف استخر خودکار (`max_links: 0`) در راه‌اندازی جدید؛ مهاجرت فقط دو خط قدیمی را به ۲/۳۲/۸ می‌برد و هرگز به auto.
- صفحهٔ Link pool که می‌گوید مقادیر کدام سرور اثر دارد و خط سقف زنده.
- یادداشت «دیده‌شدن» برای سقف بالای ۶۴ و سقف ۸ برای icmp.
- اعتبارسنجی هر پیکربندی با `hs2 check` و نصب اتمی؛ اعتبارسنج‌های ورودی برای JSON و لینک (صفر پیشرو، کاراکترهای شکنندهٔ JSON/لینک).
- تأیید sha256 با fail-closed و عبور از کش CDN؛ نگه‌داشتن باینری مشترک هنگام افزودن تونل (U1)؛ تأیید `hs2-menu` با هش هنگام دریافت از شبکه.
- ارتقای تک‌تونل یا همه با فهرست و تأیید (U2) و بررسی اتصال دوباره پس از هر بازراه‌اندازی.
- اثبات واقعی اتصال: ping روی tun برای هر حامل دارای رابط؛ شمار لینک احرازشده فقط برای mtcp.
- برچسب «✗ no peer» در فهرست با کش ۱۵ ثانیه.
- پشتیبان خودکار پیش از هر راه‌اندازی/ارتقا/حذف، هرس ۱۰تایی، بازگردانی با گسست تمیز باینری.
- مهاجرت: بذر cover یکتا، حذف sysctl ایستا، BBR پایدار، تمدید certbot با hot-reload و ۳۰ روز.
- ویرایش امن (پوشهٔ خصوصی، جارو، رد `:cq`، diff، بازگشت، هشدار فیلدهای دوطرفه).
- صفحهٔ Tuning: auto/manual/off، cc، qdisc، پیش‌تنظیم‌های سه‌گانه.
- صفحهٔ Ports (درگاه کاربر، UDP، `port_map`، پنل پیش‌فرض) با همه-یا-هیچ.
- مدیریت گواهی: سه روش صدور، هشدار DNS، وضعیت تمدید دقیق، آزمون خشک، تمدید فوری، تمدید دستی DNS-01.
- `hs2 doctor` از منو؛ `hs2 cleanup` پس از حذف و ارتقا.
- unit مقاوم: بی‌تسلیم، `modprobe tun`، `ExecReload` با SIGHUP، باز کردن ماسک.
- مانیتور زنده با نوار سهم لینک‌ها، refill، سلامت datagram، CPU سرور و PSI.
- `HS2_YES` برای اجرای بی‌ناظر؛ پرسش‌ها بی‌tty پیش‌فرض امن دارند.

---

## ۱۲. ایده‌هایی که امتحان، رد یا به تعویق افتاده‌اند

| ایده | وضعیت | دلیل | منبع |
|---|---|---|---|
| فایل sysctl ایستای `/etc/sysctl.d/99-hs2.conf` | **کنار گذاشته شد** | تنظیم به باینری منتقل شد تا تک‌منبع باشد و تغییر اندازهٔ VPS خودکار دیده شود | `install.sh:294-307`، `install.sh:4087-4091` |
| `CapabilityBoundingSet=CAP_NET_RAW CAP_NET_ADMIN` | **رد شد** | `CAP_SYS_ADMIN` را حذف می‌کند و tune را می‌شکند | `install.sh:816-823` |
| `systemctl reload hs2` به‌عنوان hook تمدید | **جایگزین شد** با `pkill -HUP -x hs2` | فقط تونل پیش‌فرض را می‌رساند | `install.sh:1123-1128` |
| `pkill -x hs2` برای حذف | **رد شد** | تونل‌های دیگر را هم می‌کشد | `install.sh:2374` |
| نصب بدون هش وقتی هش نیست («warn and install anyway») | **رد شد** (A1) | میان‌راه می‌تواند هش را بیندازد | `install.sh:316-326`؛ CHANGELOG F4 |
| `trap … RETURN` برای پاک‌سازی ویرایش | **رد شد** | پس از بازگشت تابع باز هم اجرا می‌شد و منو را می‌کشت | `install.sh:3083-3084`؛ آزمون `tm_edit_test.sh` |
| ویرایش در `/tmp` | **رد شد** | کلید و فایل‌های جانبی ویرایشگر باقی می‌ماندند | `install.sh:3077-3085` |
| پیش‌فرض Enter=همهٔ IPها برای درگاه کاربر روی سرور چند IP | **عوض شد** (U9) | باز شدن ناخواستهٔ درگاه روی همهٔ آدرس‌ها | `install.sh:247-259` |
| گشادکردن `ip_local_port_range` | **رد شد** (در باینری) | درگاه‌های پنل را موقتی می‌کرد؛ نسخهٔ قدیمی را برمی‌گرداند | `hs2-src/tune/tune.go:363-366،401-407` (توضیح در 363-366، بازگردانی در Apply) |
| گزارش «ready» بر اساس سرویس در حال اجرا | **رد شد** | gre/ipip فیلترشده سرویس را بالا نگه می‌دارد و هیچ نمی‌برد | `install.sh:898-912` |
| شمار لینک به‌عنوان اثبات اتصال برای حامل دارای tun | **رد شد** | مرز ایران دست‌دادن را رد می‌کند و سپس جریان را می‌کشد | `install.sh:937-941`؛ آزمون `tun_ports_test.py` |
| `bail` وقتی همتا وصل نشد | **عوض شد** (#5/U12) | مسیر کند یا همتای در حال آمدن؛ سرویس خودش ادامه می‌دهد | `install.sh:905-911` |
| انتقال خودکار تونل‌های قدیمی به auto یا dgtun در ارتقا | **عمداً انجام نمی‌شود** | تغییر پروتکل باید هم‌زمان دو طرف باشد؛ ورود به auto اختیاری | `install.sh:4018-4020`، `install.sh:4032-4036` |
| U4: کلید جدید + بازسازی لینک | **پیاده نشد / کنار گذاشته شد** | پیکربندی endpoint عمومی و دامنهٔ شنونده را نگه نمی‌دارد؛ به درخواست نگه‌دارنده «برای کاربران زیادی پیچیده» | CHANGELOG A4 و فاز F |
| B2: چند IP برای یک تونل (U10) | **به تعویق** به درخواست نگه‌دارنده | — | CHANGELOG B2 |
| خواندن کلیدهای نقش با باینری (#10) | **عملاً پیاده نشد**؛ grep وابسته به مقدار | `hs2 config get` این کلیدها را ندارد | `install.sh:2506-2515` |
| صفحهٔ cover یکسان برای همه | **رفع شد** با `cover_seed` | امضای هش مشترک | `install.sh:1852-1855`، `install.sh:4047-4085` |

---

## ۱۳. محدودیت‌های شناخته‌شده و مشاهده‌ها

(برچسب «مشاهده» = چیزی که در خواندن کد دیدم؛ پیشنهاد تغییر نیست.)

1. **مشاهده — آزمون e2e کهنه است (به احتمال زیاد شکست می‌خورد).** `drv.py` انتظار متن‌هایی را دارد که دیگر در `install.sh` نیستند: `"Panel inbound address"` (`hs2-src/install/e2e/drv.py:130,506`؛ متن فعلی `Default panel inbound on this server`، `install.sh:1559`)، `"Remove the hs2 tunnel"` و `"hs2 removed"` (`drv.py:340,473-474`؛ فعلی `Type REMOVE ALL…` و `All hs2 tunnels removed…`، `install.sh:2429،2439`)، `"Restored and running"` با R بزرگ (`drv.py:488`؛ فعلی `$u restored and running.`، `install.sh:3997`). همچنین به پرسش زیرشبکه (`ask_subnet`) و `Proceed? [y/N]` ارتقا پاسخ نمی‌دهد (`drv.py:101-103,432-433`). آخرین تغییر `drv.py` ثبت `5fb86a9` است. CHANGELOG فقط بازسازی `tun_ports_test.py` و `multi_tunnel_test.py` را ذکر می‌کند. (اجرا نشد؛ نتیجهٔ قطعی نامطمئن.)
2. **مشاهده — دو راه ساخت l3mtcp با رفتار متفاوت**: راه «tcp → l3mtcp» MTU ثابت 1380 و نام رابط خودکار و درگاه کاربر اجباری دارد؛ راه «tun → tcp → mtcp+tun» MTU پیش‌فرض 1320 و پرسش رابط و درگاه اختیاری (بخش ۳-۳).
3. **مشاهده — در l3mtcp اثبات اتصال نصب‌کننده فقط ping روی کانال جانبی hs0 است** (`install.sh:941-951`)؛ اگر کانال جانبی ایراد داشته باشد ولی جریان‌های درگاه کاربر سالم باشند، `verify_tunnel` و برچسب‌ها «NOT connected» می‌گویند؛ برعکسش هم ممکن است (tun سالم، مسیر پنل خراب). نامطمئن که در عمل رخ دهد.
4. **مشاهده — در direct روی ایران `PUBIP` هرگز پرسیده نمی‌شود**، پس `ask_user_ip` پیش‌فرض ندارد و Enter یعنی همهٔ IPها (IPv4 و IPv6) حتی روی سرور چند IP (`install.sh:254-266` با `iran_dialer` در `install.sh:2082-2091`). پیش‌فرض PUBIP فقط در reverse (ایران شنونده) اعمال می‌شود.
5. **مشاهده — گزینهٔ پیش‌فرض منوی Transport «auto (recommended)» است** (`install.sh:1390،1396`) که تونل IP بدون درگاه کاربر و بدون `expose` می‌سازد؛ کاربری که فقط Enter بزند به پنل نمی‌رسد (هشدار در انتهای راه‌اندازی، `install.sh:2190`).
6. **مشاهده — پشتیبان drop-inهای systemd را شامل نمی‌شود** (`install.sh:3867-3883`)، در حالی که `remove_tunnel` آن‌ها را حذف می‌کند (`install.sh:2391-2396`)؛ بنابراین حذف و سپس بازگردانی، تنظیماتی مثل `Environment=HS2_ICMP_SUPPRESS=…` یا `HS2_ICMP_CAMO=1` را برنمی‌گرداند. `/etc/modules-load.d/hs2.conf` هم در پشتیبان نیست.
7. **مشاهده — بازگردانی پشتیبان قدیمی ممکن است `/etc/sysctl.d/99-hs2.conf` را برگرداند و با `sysctl -p` اعمال کند** (`install.sh:3883`، `install.sh:3990`)، یعنی همان منبع دوم ایستایی که طراحی فعلی حذف می‌کند (تا ارتقا/راه‌اندازی بعدی).
8. **مشاهده — sysctlهای زمان اجرا (rp_filter=2، بافرها، `tcp_tw_reuse=1`، cc، qdisc) هنگام توقف یا حذف برگردانده نمی‌شوند**؛ چون پایدار نوشته نمی‌شوند تا راه‌اندازی دوبارهٔ سیستم می‌مانند. (بر پایهٔ خواندن `tune.go`؛ تنها بازگردانی، `ip_local_port_range` قدیمی است.)
9. **مشاهده — نصب‌کننده و باینری هیچ فایروال، NAT، `ip_forward` یا مسیر جز `/32` همتا تنظیم نمی‌کنند**؛ رابط tun در l3mtcp صرفاً نقطه‌به‌نقطه است و هر مسیریابی/فوروارد به عهدهٔ اپراتور است. باز بودن درگاه تونل/کاربر در فایروال هم فقط در پیام عیب‌یابی گفته می‌شود (`install.sh:1017`).
10. **مشاهده — پیام‌های گواهی با رفتار واقعی کاملاً هم‌خوان نیستند**: کلاینت گواهی را وارسی نمی‌کند (`InsecureSkipVerify: true`، `hs2-src/tlscarrier/carrier.go:191`؛ همین در `install.sh:1171-1175` گفته شده) ولی `cert_existing` می‌گوید «TLS auth will fail» (`install.sh:1349`) و `tunnel_down_help` می‌گوید گواهی باید برای دامنه معتبر باشد (`install.sh:1010-1011`).
11. **مشاهده — باینری محلی جایگزین (وقتی GitHub در دسترس نیست) با sha256 تأیید نمی‌شود**، فقط `hs2_is_v3` (`install.sh:426-429`). sha256 هم امضا نیست؛ هر کسی که مخزن را تغییر دهد هر دو را عوض می‌کند (`install.sh:309-315`).
12. **مشاهده — لینک `hs2://` کلید مشترک را به‌صورت base64 آشکار حمل می‌کند** (`install.sh:1697`)؛ هر کسی لینک را ببیند کلید را دارد.
13. **مشاهده — ارتقا تونل‌ها را پشت‌سرهم بازراه‌اندازی و هر کدام را تا ۶۰ ثانیه بررسی می‌کند** (`install.sh:4161-4182`)؛ با چند تونل که همتایشان هنوز قدیمی است، کل ارتقا چند دقیقه طول می‌کشد. همچنین هر ارتقا `apt-get update` را اجرا می‌کند (`install.sh:281`).
14. **مشاهده — اگر راه‌اندازی یک تونل جدید (نه جایگزینی) در `start_service` شکست بخورد**، `bail` انجام می‌شود ولی واحد فعال‌شده و پیکربندی می‌مانند و systemd هر ۳ ثانیه دوباره تلاش می‌کند (`install.sh:887-897`).
15. **مشاهده — شمار مجاز MTU در نصب‌کننده (68..65535) از بازهٔ هشدار `hs2 check` (576..9000، پیام «normal range 1200-1500») گشادتر است** (`install.sh:1660-1672`، `hs2-src/cmd/hs2/check.go:344-345`)؛ توضیح نصب‌کننده «WARNs outside 1200-1500» دقیقاً با شرط کد یکی نیست.
16. **مشاهده — درگاه‌های کاربر در راه‌اندازی فقط از نظر TCP آزاد بودن بررسی می‌شوند** حتی وقتی UDP از لینک روشن است (`install.sh:2124-2127`)؛ در صفحهٔ Ports این بررسی UDP وجود دارد (`install.sh:3434`، `install.sh:3466-3468`). تکرار درگاه در راه‌اندازی رد نمی‌شود (فقط WARN باینری).
17. **مشاهده — `jget`/`jraw` تجزیهٔ JSON با grep اند** و اولین تطبیق کلید را در هر عمقی برمی‌دارند (`install.sh:2503-2504`)؛ برای نقش‌ها نسخهٔ سخت‌شده وجود دارد ولی بقیه (مثل `carrier`، `iface`، `addr`) روی پیکربندی دست‌ویرایش‌شده می‌توانند اشتباه بخوانند.
18. **مشاهده — بستگی به apt**: نصب پیش‌نیازها و certbot و nano با `apt-get` است؛ روی توزیع غیر Debian/Ubuntu خطاها بلعیده می‌شوند (`|| true`) و فقط certbot `die` می‌کند (`install.sh:278-292`، `install.sh:1075-1080`).
19. **مشاهده — `tm_edit_test.sh` ناپایدار است**: در این اجرا مورد «real vim (:wq)» یک بار قرمز و بار دوم سبز شد (احتمالاً زمان‌بندی pty/vim). CHANGELOG پیش‌تر یک ناپایداری دیگر همین آزمون را (جاروی پوشه) رفع کرده بود.
20. **مشاهده — `multi_ip_test.sh` می‌گوید «lists all six public IPs»** ولی یکی از شش IP شمرده‌شده همان `10.77.0.1` رابط `hs0` است، چون stub آزمون رابط tun را معرفی نمی‌کند (`hs2-src/install/tests/multi_ip_test.sh:13-23,31-33`). خود تابع درست است؛ فقط نام بررسی دقیق نیست.
21. محدودیت مستند: پشتیبان‌ها کلید تونل و کلید خصوصی گواهی را دارند (`install.sh:2447-2450`؛ CHANGELOG «Known limitations»). گواهی DNS-01 دستی خودبه‌خود تمدید نمی‌شود (`install.sh:1158-1164`).

---

## ۱۴. ارجاع به زیرسیستم‌های دیگر

**نصب‌کننده این فرمان‌های باینری را صدا می‌زند** (توزیع در `hs2-src/cmd/hs2/main.go:220-248`):

| فرمان | محل فراخوانی در install.sh | کار در باینری |
|---|---|---|
| `hs2 version` | `install.sh:329،343،370،456،3890،4000،4203` | `hs2-src/cmd/hs2/main.go:127،221-222` (رشتهٔ `hs2 v3 (…)` + `[build …]`) |
| `hs2 check -c` | `install.sh:866،3005،4066،4077` | `hs2-src/cmd/hs2/check.go:31-55`؛ خروج ۱ فقط برای ERROR |
| `hs2 config -c … set` | `install.sh:3171` | `hs2-src/cmd/hs2/config.go:52-58،83-95` |
| `hs2 tune -c` | `install.sh:3185،3191` | `hs2-src/cmd/hs2/main.go:573-592` |
| `hs2 recommend-links [--why] [-c]` | `install.sh:1763،1781،1814،3285-3288` | `hs2-src/cmd/hs2/main.go:604-630`، `hs2-src/tune/tune.go:229-251` |
| `hs2 ports -c …` | `install.sh:1531،3366،3415` | `hs2-src/cmd/hs2/ports.go` |
| `hs2 doctor -c` | `install.sh:3615` | `hs2-src/cmd/hs2/doctor.go` |
| `hs2 cleanup` | `install.sh:2408` | `hs2-src/cmd/hs2/cleanup.go:20-32` |
| `hs2 run -c` (از unit) | `install.sh:836` | `hs2-src/cmd/hs2/main.go:322-424` |

**نصب‌کننده این خروجی‌های باینری را می‌خواند**: فایل وضعیت `/run/hs2/*.status.json` (هر ۲ ثانیه، `hs2-src/cmd/hs2/status.go:29،182-191`) و نام فایل `.warm` (`status.go:195-201`، کهنگی ۱۵ دقیقه).

**قراردادهای مشترک**: SIGHUP = بارگذاری دوبارهٔ گواهی (`main.go:387-401`)؛ نگاشت `max_links` سه‌حالته (`main.go:678-711`)؛ پروفایل‌های tune (`tune.go:159-170،280-289`)؛ رابط tun (`tun/tun_linux.go:100-130`)؛ قواعد icmp (`encap/echoguard_linux.go`).

**اسناد هم‌سایه در همین پوشه**: `01-cmd-runtime.md` (اجرای `hs2 run` و حامل‌ها)، `02-cmd-ops-tune.md` (check/doctor/status/tune/config/ports به تفصیل)، `04-linkmanager.md` و `05-autopilot-health.md` (استخر لینک و `min/max/per_link` که نصب‌کننده می‌نویسد)، `06-l3-tun.md` (کانال جانبی hs0 در l3mtcp و tls)، `08-dgtun.md` (حامل‌های datagram و MTU 1280)، `10-encap.md` (icmp و قواعد echo-guard).

**از کجا صدا زده می‌شود**: کاربر با `bash install.sh` یا `curl … | bash [-s upgrade|manage]` یا `bash <(curl …)`؛ پس از نصب با `hs2-menu`؛ certbot از طریق `renew_hook`/`deploy-hook` که `pkill -HUP -x hs2` را اجرا می‌کند؛ systemd از طریق unit نوشته‌شده.
