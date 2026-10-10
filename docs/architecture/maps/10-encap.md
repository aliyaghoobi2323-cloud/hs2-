# encap: سوکت خام، ICMP و استتار

> وضعیت: شاخهٔ فعلی برابر `main` (ثبت `0812bc9`، باینری build `da621f5db2bb`). همهٔ مسیرها نسبت به `/home/user/hs2-/hs2-src` هستند مگر خلافش گفته شود (`README.md`، `CHANGELOG.md` و `install.sh` در ریشهٔ `/home/user/hs2-` هستند؛ `VALIDATION.md` در `hs2-src`).
> هیچ فایلی تغییر داده نشد. آزمون‌ها اجرا شدند: `go test ./encap/` سبز (۵۱ قبول، ۳ رد شده به‌خاطر نبودن `ip`، `ping` و `tc` در این محیط)، و آزمون‌های `Camo|Encap|InnerMTU|CarrierOverhead` در `udpcarrier`، `Echo|Mute|ICMP` در `engine` و `ICMP` در `cmd/hs2` هم سبز بودند.

---

## ۰. پاسخ کوتاه به پرسش اصلی: آیا این‌ها به l3mtcp ربط دارند؟

**نه، مستقیماً ربطی ندارند.** بستهٔ `encap` و استتار ICMP فقط در مسیر حامل‌های datagram (`udpcarrier`) به کار می‌روند:

| حامل (`carrier`) | از encap استفاده می‌کند؟ | چطور |
|---|---|---|
| `dgtun` | **بله، با هر نوع کپسول** (`udp`، `icmp`، `gre`، `ipip`، `ipx`) | `cmd/hs2/main.go:853` مقدار `EncapConfig{Kind: fc.Encap, ...}` را می‌سازد، سپس `engine.NewDgDialer`/`NewDgListener` (`engine/dgcarrier.go:25-52`) ← `udpcarrier.DialCfg`/`ListenCfg` ← `encap.Dial`/`encap.Listen` (`udpcarrier/dial.go:88`، `udpcarrier/listen.go:69`) |
| `udp`، `auto` | فقط کپسول `udp` (سوکت UDP با بافر ۴ مگابایت) | `engine/carrier_udp.go:35,44,72` ← `udpcarrier.DialFrom`/`Listen` با `EncapConfig` تهی، یعنی kind برابر `udp` |
| `l3mtcp`/`l3`، `mtcp`، `tls` | **خیر** | `runStream` (`cmd/hs2/main.go:453-548`) فقط `tlscarrier` (TCP/TLS) و TUN کانال جانبی را باز می‌کند؛ هیچ فایلی در `engine` به‌جز `dgcarrier.go`، `dgpool.go` و `carrier_udp.go`، بستهٔ `udpcarrier` را وارد نمی‌کند و `engine` اصلاً `encap` را وارد نمی‌کند |
| `reality`، `noise` | خیر | — |

تنها تماس l3mtcp با encap در سطح پردازه است و اثری ندارد: `defer encap.ReleaseAllEchoGuards()` در `runCmd` (`cmd/hs2/main.go:325`)، در goroutine خروج اجباری (`main.go:383`) و در `must()` (`main.go:1008`). وقتی هیچ شنوندهٔ ICMP وجود ندارد، این فراخوانی هیچ کاری نمی‌کند. فیلد پیکربندی `encap` برای l3mtcp نادیده گرفته می‌شود: `hs2 check` آن را فقط برای `dgtun` بررسی می‌کند (`cmd/hs2/check.go:155,297-310`). در نصب‌کننده، گزینهٔ «tun → ۶) tcp → ۱) mtcp + tun» همان l3mtcp است، ولی گزینه‌های ۱ تا ۵ (udp/icmp/gre/ipip/ipx) به `dgtun` می‌روند (`install.sh:1405-1446`). خود نصب‌کننده و README هم می‌گویند TUN در l3mtcp «کانال جانبی برای پینگ و ترافیک سبک است؛ برای ترافیک حجیم روی tun مسیریابی‌شده، tun → udp یا icmp را بگیرید» (`install.sh:1442-1443`، `README.md:659-666`).

نتیجه برای ایده‌پردازی: هر ایده‌ای دربارهٔ استتار ICMP، echoguard یا سوکت خام، فقط روی `dgtun` (و تا حدی روی `udp`/`auto` برای بخش UDP) اثر می‌گذارد، نه روی l3mtcp.

---

## ۱. نقش و جایگاه در کل سیستم

- `encap` لایهٔ **انتقال datagram** زیر حامل UDP (`udpcarrier`) است. حامل هر قاب را با Noise IKpsk2 و ChaCha20-Poly1305 و FEC تطبیقی مهر و موم می‌کند و به‌صورت یک datagram مات به encap می‌دهد. encap فقط تعیین می‌کند این datagram **چطور** از سیم عبور کند (`encap/encap.go:1-31`).
- رابط آن، رابط‌های استاندارد Go است: سمت شماره‌گیر `net.Conn` و سمت شنونده `net.PacketConn` (`encap/encap.go:19-30`). پس حامل کد ویژهٔ کپسول ندارد. استثنا چند رابط اختیاری است که حامل با type assertion پیدا می‌کند: `WriteBatch`، `SetReceiver`، `ReadBatch` و `WriteBatchTo` (`udpcarrier/dial.go:102-122`، `udpcarrier/socket.go:38-46`).
- پنج نوع کپسول وجود دارد (`encap/encap.go:43-49`):
  - `udp`: سوکت عادی UDP (پیش‌فرض، بی‌نیاز از دسترسی ویژه)؛
  - `icmp`: payload یک بستهٔ ICMP Echo (پروتکل IP شمارهٔ ۱)؛
  - `gre`: پروتکل ۴۷، طبق RFC 2890 همراه با key؛
  - `ipip`: پروتکل ۴؛
  - `ipx`: یک پروتکل IP خام با شمارهٔ انتخابی (پیش‌فرض ۲۵۳).
- نوع‌های خام به `CAP_NET_RAW` (root) نیاز دارند و فقط روی لینوکس کار می‌کنند (`encap/raw.go:34-55`، `encap/raw_other.go`).
- چون payload همیشه datagram مهرشدهٔ حامل است، **همهٔ کپسول‌ها رمزنگاری، احراز اصالت و FEC یکسانی دارند**. کپسول فقط پاکت بیرونی است.
- نقش‌ها در ICMP: سمتی که شماره می‌گیرد echo **request** (نوع ۸) می‌فرستد و سمت شنونده echo **reply** (نوع ۰) (`encap/rawframe.go:84-87`).
  - در حالت مستقیم (direct)، لبهٔ ایران شماره می‌گیرد، پس درخواست می‌فرستد؛ خروجی شنونده است و پاسخ می‌فرستد. echoguard روی سرور خارج نصب می‌شود.
  - در حالت معکوس (reverse)، خروجی شماره می‌گیرد و لبهٔ ایران شنونده است، پس echoguard روی سرور ایران است (`cmd/hs2/main.go:905-907`).
- **استتار ICMP (فاز CA)** دو بخش دارد که در دو بسته پخش شده‌اند:
  - **CA2** (شناسهٔ شبه‌PID) در `encap/raw_linux.go`؛
  - **CA1 و CA1b** (زمان‌بندی feedback و jitter کاوش) در `udpcarrier/carrier.go` و `udpcarrier/rate.go`.
- **شکل‌دهی echo نسبت ۱:۱ (B5)** در `engine/dgpool.go` است، نه در encap.

---

## ۲. اجزای اصلی (نوع‌ها، توابع، goroutineها)

### ۲.۱ سطح عمومی (`encap/encap.go`، `encap/raw.go`، `encap/udp.go`)
| جزء | محل | توضیح |
|---|---|---|
| `Options{BindIP, ICMPRole, Proto, Key}` | `encap/encap.go:53-75` | `Key` همان shared secret تونل است (`udpcarrier/dial.go:28-34`). `ICMPRole` پر می‌شود ولی **هیچ‌جا خوانده نمی‌شود** (نقش از dial/listen می‌آید؛ مشاهدهٔ ۱۳.۹) |
| `Dial(kind, addr, opt)` / `Listen(...)` | `encap/encap.go:113-148` | `normalize` نام را کوچک و trim می‌کند و رشتهٔ تهی را `udp` می‌گیرد (`encap/encap.go:191-197`). برای کپسول خام پسوند `:port` پذیرفته و نادیده گرفته می‌شود (`hostOf`، `encap/encap.go:202-207`) |
| `Overhead(kind)` | `encap/encap.go:154-169` | udp=8، icmp=16، gre=8، ipip=4، ipx=4 (`encap/raw.go:26-29`) |
| `ValidIPXProto` / `ipxHandledProtos` | `encap/encap.go:88-108` | شمارهٔ پروتکل باید ۱ تا ۲۵۴ باشد و هسته handler نداشته باشد. نسخهٔ آینه‌ای در `cmd/hs2/check.go:79-86` هست |
| `dialUDP` / `listenUDP` | `encap/udp.go:26-58` | `setSockBufs(c, sockBuf)` با `sockBuf = 4<<20` (`encap/udp.go:24`). شنونده روی `udp4` است تا pktinfo برای IPv4 کار کند (`encap/udp.go:64-69`) |
| `forceSockBufs` | `encap/sockbuf_linux.go:14-21` | ابتدا `SO_RCVBUFFORCE`/`SO_SNDBUFFORCE`، و اگر نشد `SO_RCVBUF`/`SO_SNDBUF` |
| `dialRawFn` / `listenRawFn` | `encap/raw.go:38-55`، `encap/raw_linux.go:60-63` | روی لینوکس با `init` پر می‌شوند |

### ۲.۲ قاب‌بندی (`encap/rawframe.go`، مستقل از سکو)
| جزء | محل | توضیح |
|---|---|---|
| `Addr{IP, ID, Kind}` | `encap/rawframe.go:26-38` | `String()` برابر `"IP#id"` است و کلید demux شنوندهٔ حامل همین است (`udpcarrier/listen.go:134`) |
| `framer` | `encap/rawframe.go:57-76` | برای هر نوع و هر سمت. فیلدهای ICMP: `obf`، `obfKey`، `txPrefix`/`rxPrefix` و `noncer` |
| `newFramer` | `encap/rawframe.go:78-119` | ICMP همیشه obfuscate می‌شود. پیشوند c2s/s2c را از `obfSeqPrefixes` می‌گیرد. نوع‌های دیگر magic را از `framingMagics` می‌گیرند |
| `framingMagics` | `encap/rawframe.go:134-145` | `HMAC-SHA256(key, "hs2-encap-magic-v1\0"+kind+"\0"+proto+"\0"+dir)[:2]`. اگر دو جهت برابر شوند، `s2c ^= 0xffff` |
| `build` / `parse` | `encap/rawframe.go:149-212` | ساخت و اعتبارسنجی سرآیند انتقال. در ICMP، `parse` ماسک را **درجا** باز می‌کند (`encap/rawframe.go:197-199`) |
| `ipv4Payload` | `encap/rawframe.go:218-233` | IPv4 کامل و غیرتکه‌تکه با پروتکل درست را می‌پذیرد و IHL را رعایت می‌کند. قطعه‌ها را (MF یا offset ناصفر) رد می‌کند |
| `inetChecksum` | `encap/rawframe.go:237-250` | چک‌سام RFC 1071 (سوکت خام ICMP آن را پر نمی‌کند) |
| `recvFilter(id, srcIP)` | `encap/rawframe.go:281-317` | فیلتر BPF کلاسیک درون هسته (جزئیات در §۷.۵) |

### ۲.۳ مبهم‌سازی ICMP (`encap/obfs.go`)
| جزء | محل | توضیح |
|---|---|---|
| `obfKeyFromSecret` | `encap/obfs.go:74-78` | BLAKE2s-256 که با secret کلیددار می‌شود؛ برچسب `"hs2-icmp-obfs-key-v1"` |
| `obfSeqPrefixes` | `encap/obfs.go:85-109` | BLAKE2s کلیددار با `"hs2-icmp-obfs-seq-v1\0"+dir`. بایت بالا پیشوند جهت است. اگر صفر شد، `0x0100` می‌شود. اگر دو جهت برابر شدند، بیت‌های پیشوند برگردانده می‌شوند |
| `obfSeq` / `obfSeqHasPrefix` | `encap/obfs.go:113-121` | پیشوند در ۸ بیت بالا و شمارنده در ۸ بیت پایین |
| `obfNoncer` | `encap/obfs.go:133-157` | بافر ۸×۵۱۲ بایتی که هر ۵۱۲ بسته یک بار از `crypto/rand` پر می‌شود و با mutex محافظت می‌شود |
| `obfMask` | `encap/obfs.go:161-169` | ChaCha20 با کلید ۳۲ بایتی و nonce دوازده‌بایتی (۴ بایت صفر و سپس nonce هشت‌بایتی بسته). خودش وارون خودش است |
| `nonEmpty` | `encap/obfs.go:173-181` | secret تهی را `{0}` می‌کند و secret بلندتر از ۳۲ بایت را برای کلید BLAKE2s به ۳۲ بایت کوتاه می‌کند |

### ۲.۴ سمت شماره‌گیر: سوکت مشترک (`encap/raw_linux.go`)
| جزء | محل | توضیح |
|---|---|---|
| `rawMuxKey{kind, proto, key, bind, peer}` | `encap/raw_linux.go:165-171` | برای هر ترکیب (نوع، کلید، IP مبدأ، IP همتا) فقط **یک** سوکت دریافت وجود دارد |
| `rawIDKey{kind, peer}` و `rawIDs` | `encap/raw_linux.go:176-185` | یکتایی شناسهٔ لینک در گسترهٔ (نوع، IP همتا) و در کل پردازه |
| `rawMux` | `encap/raw_linux.go:188-202` | سوکت مشترک، framer، `tx *rawTx`، کانال `dead`، `links map[uint16]*rawConn`، `refs`، و `dropped` |
| `rawConn` (یک لینک) | `encap/raw_linux.go:205-230` | `id`، `seq` با شروع تصادفی (`encap/raw_linux.go:286-288`)، صف `q` به ظرفیت ۵۱۲، `recv` (پس از `SetReceiver`)، `rdl` برای مهلت خواندن، و آرایه‌های `WriteBatch` |
| `dialRawLinux` | `encap/raw_linux.go:232-294` | باز کردن یا گرفتن mux، تخصیص شناسه (در حالت camo: `camoLinkID`، `encap/raw_linux.go:273-276`) و ثبت لینک |
| `uniqueLinkID` | `encap/raw_linux.go:298-306` | شناسهٔ تصادفی ناصفر از `crypto/rand` که در حال استفاده نباشد |
| `camoLinkID` (CA2) | `encap/raw_linux.go:42-49` | `camoIDNext += 1+IntN(40)` و `id = camoIDBase + camoIDNext` |
| `openRawMux` | `encap/raw_linux.go:310-336` | `net.ListenIP` **بدون اتصال**، تنظیم سوکت، `openRawTx` و `go mx.readLoop()` |
| **goroutine** `rawMux.readLoop` | `encap/raw_linux.go:349-396` | برای هر سوکت مشترک یک goroutine. با `recvmmsg` در دسته‌های ۳۲تایی می‌خواند. خطای نرم نادیده گرفته می‌شود و خطای سخت `fail` را صدا می‌زند |
| `deliver` | `encap/raw_linux.go:403-430` | منبع باید همتا باشد، سپس `parse`، سپس لینک با شناسهٔ درست. پیش از `SetReceiver` بسته در صف `q` می‌رود (اگر صف پر باشد دور ریخته و در `dropped` شمرده می‌شود) و پس از آن مستقیماً به `recv` داده می‌شود |
| `Write` / `WriteBatch` / `writeBatchTx` | `encap/raw_linux.go:496-618` | مسیر `rawTx` اگر سالم باشد، وگرنه مسیر قدیمی |
| `sendAll` (مسیر قدیمی) | `encap/raw_linux.go:621-635` | `sendmmsg`؛ در خطای نرم فقط همان datagram دور ریخته و در `sendRefused` شمرده می‌شود |
| `Close` / `releaseLocked` | `encap/raw_linux.go:639-666`، `444-452` | آزاد کردن شناسه؛ آخرین لینک سوکت را می‌بندد |
| `SetReceiver` | `encap/raw_linux.go:672-698` | صف را تخلیه و به تابع مستقیم سوئیچ می‌کند. اگر سوکت مشترک خراب شود، `onErr` فراخوانی می‌شود |
| `rawMuxStats` | `encap/raw_linux.go:722-730` | فقط برای آزمون‌ها صادر شده است (`encap/export_linux_test.go:6`) |

### ۲.۵ سمت شنونده (`encap/raw_linux.go`)
| جزء | محل | توضیح |
|---|---|---|
| `rawPeer{addr, replyCtr, local, seen}` | `encap/raw_linux.go:741-751` | `replyCtr` شمارندهٔ **خودِ شنونده** برای seq پاسخ‌هاست |
| `rawPacketConn` | `encap/raw_linux.go:753-772` | `ipc`، `tx`، `wildcard`، `guardKey` (بایت پیشوند c2s)، `peers` |
| `listenRawLinux` | `encap/raw_linux.go:784-838` | برای ICMP، **پیش از باز کردن سوکت** `acquireEchoGuard(guardKey)` را می‌گیرد (`encap/raw_linux.go:804-813`) |
| `ReadFrom` / `ReadBatch` | `encap/raw_linux.go:840-912` | `recvmmsg` به اندازهٔ ۳۲×۹۲۱۶، سپس `ipv4Payload`، `parse` و `notePeer` |
| `notePeer` | `encap/raw_linux.go:1008-1041` | ثبت همتا **پیش از AEAD**. شمارنده با بذر تصادفی شروع می‌شود و `local` (مقصد بسته) برای شنوندهٔ wildcard ذخیره می‌شود. هر دقیقه sweep اجرا می‌شود |
| `WriteTo` / `WriteBatchTo` | `encap/raw_linux.go:1051-1090`، `924-1001` | `replyCtr++` (در دسته‌ای: `+= len(chunk)`) و ارسال از `local` |
| `legacyWriteTo` | `encap/raw_linux.go:1097-1114` | اگر `local` تعیین شده باشد، ارسال با `IP_PKTINFO` |
| `Close` | `encap/raw_linux.go:1116-1123` | بستن سوکت، بستن `tx`، و `releaseEchoGuard` فقط یک بار (`closeEcho`) |

### ۲.۶ سوکت فقط‌ارسال (`encap/rawtx_linux.go`، فاز X5)
| جزء | محل | توضیح |
|---|---|---|
| `rawTx{ipc, rc, tmpl[20], df, protoCmsg, noProto, broken}` | `encap/rawtx_linux.go:53-62` | یک سوکت `IPPROTO_RAW` (`"ip4:255"`) کنار سوکت دریافت ICMP |
| `openRawTx` | `encap/rawtx_linux.go:80-156` | فقط برای ICMP. TTL، TOS و `IP_MTU_DISCOVER` را از سوکت دریافت می‌خواند. فیلتر BPF «همه را دور بریز» و `IP_RECVERR` را تنظیم می‌کند. اگر حالت WANT باشد، `nil` برمی‌گرداند |
| `frame` | `encap/rawtx_linux.go:185-200` | سرآیند IPv4 را از الگو می‌سازد و مبدأ و مقصد را پر می‌کند (طول کل و checksum را هسته پر می‌کند) |
| `sendAll` | `encap/rawtx_linux.go:222-277` | `SendNoLock` (بدون قفل Go). ماشین حالت خطا در §۶ آمده است |
| `txSocketErr` | `encap/rawtx_linux.go:160-171` | خطای خودِ سوکت (نه خطای یک datagram) |

### ۲.۷ نگهبان echo (`encap/echoguard_linux.go`)
| جزء | محل | توضیح |
|---|---|---|
| `echoGuard{refs, method, release}` و `guards map[uint16]` | `encap/echoguard_linux.go:41-50` | کلید آن بایت پیشوند c2s است |
| `acquireEchoGuard` / `releaseEchoGuard` | `encap/echoguard_linux.go:55-83` | شمارش ارجاع؛ آخرین ارجاع قاعده را برمی‌دارد |
| `installEchoGuard` | `encap/echoguard_linux.go:110-150` | ترتیب امتحان: `nft`، سپس `iptables`، سپس `global`. با `HS2_ICMP_SUPPRESS` می‌شود یکی را اجبار کرد. sweep یک‌باره در هر پردازه هم اینجاست |
| `nftGuardInstall` / `nftGuardRemove` | `encap/echoguard_linux.go:154-178` | جدول خصوصی `inet hs2_icmp_%04x_<pid>` |
| `iptGuardInstall` / `iptGuardRemove` | `encap/echoguard_linux.go:182-214` | قاعدهٔ `OUTPUT` با `u32` و comment به شکل `hs2-icmp-%04x-<pid>` |
| `SweepStaleEchoGuards(legacy)` | `encap/echoguard_linux.go:267-305` | قاعده‌های پردازه‌های مرده را برمی‌دارد، و نیز نشانگرهای حالت global |
| `IsHs2Daemon` / `ownerAlive` | `encap/echoguard_linux.go:220-236` | `/proc/<pid>/cmdline`: نام پایهٔ اجرایی با `hs2` شروع شود و آرگومان دوم `run` باشد |
| `acquireEchoIgnore` / `releaseEchoIgnore` | `encap/raw_linux.go:1148-1182` | `icmp_echo_ignore_all` با شمارش ارجاع. فقط مقداری را برمی‌گرداند که خودش عوض کرده است (`echoWeSet`) |
| `acquireEchoIgnoreMarked` و ... | `encap/echoguard_linux.go:328-379` | فایل نشانگر `/run/hs2/icmp-echo-ignore/<pid>.<netns>` |
| `ReleaseAllEchoGuards` | `encap/echoguard_linux.go:99-106` | هنگام خروج daemon همهٔ قاعده‌ها را برمی‌دارد، صرف‌نظر از شمارش ارجاع |

### ۲.۸ استتار زمانی در `udpcarrier` (CA1 و CA1b)
| جزء | محل | توضیح |
|---|---|---|
| `icmpCamo` | `udpcarrier/carrier.go:35` | `os.Getenv("HS2_ICMP_CAMO") == "1"` (جدا از `icmpCamoID` در encap: `encap/raw_linux.go:29`) |
| `camoIdlePoll = 700ms` | `udpcarrier/carrier.go:41` | پایهٔ ضربان بی‌کاری (CA1b) |
| `camoJitter(d, frac)` | `udpcarrier/carrier.go:45-47` | `d × U[1-frac, 1+frac]` با `math/rand/v2` |
| **goroutine** `feedbackLoop` | `udpcarrier/carrier.go:537-583` | بدون camo: `Ticker(100ms)`؛ با camo: timer دارای jitter (جزئیات در §۵) |
| `nextProbe` (شاخهٔ camo) | `udpcarrier/rate.go:345-365` | اگر `icmpCamo` باشد، زمان کاوش پایه با `camoProbeOffset(k+1)` جابه‌جا می‌شود |
| `camoProbeJit = 1500ms` / `camoProbeOffset` | `udpcarrier/rate.go:369-382` | آفست قطعی از هش splitmix64 روی شمارهٔ دوره، در بازهٔ [-1.5s, +1.5s] |

### ۲.۹ در engine (استخر datagram)
| جزء | محل | توضیح |
|---|---|---|
| `encapICMP = "icmp"` | `engine/dgpool.go:875` | engine خود encap را وارد نمی‌کند |
| `carrierEchoShaped` | `engine/dgpool.go:887-894` | `Encap()` حامل را با `EqualFold` و `TrimSpace` مقایسه می‌کند |
| `dgLink.echoShaped`، `echoDial`، `rxFrames`، `txFrames` | `engine/dgpool.go:136-143`، `180-184` | در `add` پر می‌شوند (`engine/dgpool.go:662-663`) |
| `echoBalance` (B5) | `engine/dgpool.go:912-925` | اگر `txFrames < rxFrames` باشد یک قاب پرکننده می‌فرستد: سمت شماره‌گیر `TypePing`، سمت شنونده `TypePong`. اندازهٔ پرکننده تصادفی در بازهٔ ۰ تا ۹۵ بایت است (`echoFillerPad [96]byte`، `engine/dgpool.go:880`) |
| **goroutine** `muteLoop` / `muteTick` | `engine/dgpool.go:1047-1140` | آشکارساز «حامل لال» که CA1 با آن تداخل داشت |
| بستن شنونده | `engine/dgpool.go:2204-2209` | `defer cfg.Listener.Close()` تا قاعدهٔ echoguard پیش از خروج آزاد شود |
| `icmpMaxLinks = 8` | `cmd/hs2/main.go:671-683` | سقف ۸ حامل برای tun روی icmp |

---

## ۳. جریان داده و کنترل، گام‌به‌گام

### ۳.۱ برپایی حامل ICMP در سمت شماره‌گیر (لبه در حالت مستقیم، خروجی در حالت معکوس)
1. استخر شماره‌گیری می‌کند: `dgUDPDialer.Dial` ← `udpcarrier.DialCfg(ctx, addr, ec, shared, mtu)` (`engine/dgcarrier.go:25-27`، `udpcarrier/dial.go:80`).
2. `encap.Dial("icmp", addr, Options{BindIP, Proto, Key: shared})` (`udpcarrier/dial.go:88`) ← `dialRawLinux` (`encap/raw_linux.go:232`):
   1. `newFramer(dial=true)` ارسال `txType=8` و دریافت `rxType=0` را تعیین می‌کند. `txPrefix` برابر c2s و `rxPrefix` برابر s2c است (`encap/rawframe.go:82-97`).
   2. آدرس همتا resolve می‌شود و باید IPv4 باشد. BindIP تجزیه می‌شود. زیر قفل `rawMuxMu`، اگر mux نبود یا خراب بود، `openRawMux` اجرا می‌شود:
      - `net.ListenIP("ip4:1", bind)`؛
      - `tuneRawSocket`: بافر ۴ مگابایت، `IP_PMTUDISC_DO` (یعنی DF=1 مانند ping)، و فیلتر BPF «منبع = همتا، نوع = ۰، بایت seq[6] = پیشوند s2c»؛
      - `openRawTx`؛
      - `go readLoop`.
      (`encap/raw_linux.go:310-336`)
   3. تخصیص شناسه: بدون camo `uniqueLinkID` و با camo `camoLinkID`. سپس `rawConn` با `q` به ظرفیت ۵۱۲ و `seq` تصادفی ساخته می‌شود (`encap/raw_linux.go:269-293`).
3. دست‌دهی Noise در `dialHandshake` (`udpcarrier/dial.go:180-223`): تا ۱۲ تلاش، انتظار هر تلاش `300+150×attempt` میلی‌ثانیه، و سقف کل ۱۰ ثانیه. مسیر نوشتن پیام ۱ این است:
   1. `rawConn.Write` ← `tx.frame`: IPv4 از الگو، ICMP با `type=8`، `id=linkID`، `seq=(c2s<<8 | ctr)`، nonce هشت‌بایتی، ماسک ChaCha20 روی ۶۴ بایت اول payload، و checksum؛
   2. `tx.sendAll` ← `SendNoLock` (`sendmmsg` روی `IPPROTO_RAW` با `IP_PROTOCOL` در cmsg).
   پیام ۲ از راه `readLoop` (`recvmmsg`) ← `deliver` ← `ipv4Payload` (منبع باید همتا باشد) ← `parse` (نوع ۰، code ۰، پیشوند s2c، و باز کردن ماسک) ← `links[id]` ← صف `q` ← `rawConn.Read` می‌رسد.
4. پس از دست‌دهی (`udpcarrier/dial.go:98-122`): `newConn`، سپس `setWriteBatch(rawConn.WriteBatch)`، سپس `SetReceiver(func(b){ c.tryFeed(copy(b)) }, func(error){ c.Close() })`. از این به بعد `readLoop` هر بسته را مستقیم به حامل می‌دهد (صف `q` دیگر استفاده نمی‌شود و ریزش آن در `Conn.rxDropped` شمرده می‌شود). در پایان `runClientConfirm` با مهلت ۶ ثانیه و ارسال دوباره هر ۱۵۰ میلی‌ثانیه اجرا می‌شود.
5. مسیر داده: pacer ← `writeBatch` (حداکثر ۱۶ datagram در یک فراخوانی، `udpcarrier/pacer.go:87`) ← `rawConn.WriteBatch` ← `writeBatchTx` (تکه‌های ۳۲تایی) ← `rawTx.sendAll`.

### ۳.۲ برپایی شنوندهٔ ICMP (خروجی در حالت مستقیم، لبه در حالت معکوس)
1. `engine.NewDgListener` ← `udpcarrier.ListenCfg` ← `encap.Listen("icmp", addr, opts)` ← `listenRawLinux` (`encap/raw_linux.go:784`):
   1. `newFramer(dial=false)` ارسال نوع ۰ و دریافت نوع ۸ را تعیین می‌کند؛ `rxPrefix` برابر c2s است.
   2. `guardKey = rxPrefix >> 8` (بایت c2s). `acquireEchoGuard(guardKey)` قاعدهٔ nft، iptables یا حالت global را نصب می‌کند (§۳.۴).
   3. `net.ListenIP("ip4:1", ip)` و `tuneRawSocket` با `IP_PMTUDISC_DONT` (یعنی DF=0 مانند پاسخ‌های خود هسته) و BPF «نوع ۸، بایت seq[6] = c2s».
   4. `openRawTx(bind)` (برای wildcard، `bind = nil` است).
2. `Listener.serve` (`udpcarrier/listen.go:93-125`) ← `packetSocket.readBatch` ← `rawPacketConn.ReadBatch` ← برای هر بسته: `ipv4Payload` ← `parse` ← `notePeer(src, dst, id)` ← `handle(payload, *Addr, nil)`.
3. `Listener.handle` (`udpcarrier/listen.go:130-156`) با کلید `"IP#id"`: اگر همتا آشنا باشد، `tryFeed` (یا بازپخش پیام ۲ از cache). اگر نشانی تازه باشد، اول `probe` و سپس `tryHandshake` (Noise `ReadMessage1Payload`).
4. پاسخ‌ها: `Conn.write` ← `Listener.send` ← `packetSocket.writeTo` ← `rawPacketConn.WriteTo` (`seq = replyCtr++` و `local` برای wildcard) ← `tx.frame(src=local)` ← `sendAll`. در حالت دسته‌ای، `writeBatchTo` ← `WriteBatchTo` (`encap/raw_linux.go:924-1001`).
5. بستن: `Listener.Close` ← `pc.Close` ← `rawPacketConn.Close`: بستن سوکت، بستن `tx` و `releaseEchoGuard`.

### ۳.۳ جریان دیگر کپسول‌ها
- **gre/ipip/ipx** همین مسیر را دارند ولی magic شانزده‌بیتی کلیددار دارند، ماسک ندارند و **`rawTx` ندارند** (`openRawTx` برای نوع غیر ICMP `nil` برمی‌گرداند: `encap/rawtx_linux.go:81`). پس ارسال همیشه روی سوکت مشترک است (قفل Go و `lock_sock` هسته). `tuneRawSocket` برای این نوع‌ها `IP_MTU_DISCOVER` را دست نمی‌زند (`pmtudiscFor` مقدار ۱- برمی‌گرداند: `encap/raw_linux.go:123-131`).
- **udp**: `net.DialUDP` یا `net.ListenUDP("udp4")` با بافر ۴ مگابایت. pktinfo، دسته‌ای کردن و `SendNoLock` در `udpcarrier` انجام می‌شوند (`udpcarrier/pktinfo_linux.go`، `udpcarrier/batch_linux.go`).

### ۳.۴ نصب و حذف echoguard
1. اولین `acquireEchoGuard(m)` در پردازه ← `installEchoGuard` ← `sweepOnce`: یک بار `SweepStaleEchoGuards(false)` اجرا می‌شود و برای هر قاعدهٔ کهنه لاگ می‌گیرد (`encap/echoguard_linux.go:112-116`).
2. `HS2_ICMP_SUPPRESS` خوانده می‌شود: `nft`، `iptables`، `global`، یا هیچ‌کدام که یعنی هر سه به ترتیب امتحان شوند (`encap/echoguard_linux.go:117-122`).
3. **nft**: `nft -f -` با تراکنش «table؛ delete table؛ table {...}» تا باقی‌ماندهٔ اجرای پیشین در همان تراکنش حذف شود:
   `table inet hs2_icmp_00XX_<pid> { chain out { type filter hook output priority 0; policy accept; icmp type echo-reply @th,48,8 0xXX drop } }` (`encap/echoguard_linux.go:160-174`).
4. **iptables**: اول `iptGuardRemove` (حداکثر ۸ حذف)، سپس `iptables -w -I OUTPUT -p icmp --icmp-type echo-reply -m u32 --u32 "0>>22&0x3C@4>>8&0xff=0xXX" -m comment --comment hs2-icmp-00XX-<pid> -j DROP` (`encap/echoguard_linux.go:191-206`).
5. **global**: `acquireEchoIgnoreMarked` ← `acquireEchoIgnore` (`icmp_echo_ignore_all=1` اگر از قبل ۱ نبود) و نوشتن فایل نشانگر. اگر به این حالت «افتاده باشد» (نه اجباری)، لاگ هشدار می‌دهد (`encap/echoguard_linux.go:138-146`).
6. حذف: آخرین `releaseEchoGuard`، یا `ReleaseAllEchoGuards` هنگام خروج (`cmd/hs2/main.go:325,383,1008`)، یا `hs2 cleanup` (`cmd/hs2/cleanup.go:20-32`).

**منطق انتخاب بایت:** پاسخی که هسته به یکی از درخواست‌های تونل می‌دهد همان `id/seq` درخواست را بازتاب می‌دهد، پس بایت بالای seq آن پیشوند c2s است و دور ریخته می‌شود. پاسخ‌های خود hs2 پیشوند s2c دارند و می‌گذرند. پینگ عادی کوتاه (seq از ۱ تا ۲۵۵) بایت بالای صفر دارد و چون پیشوند ناصفر است، هرگز گرفتار نمی‌شود (`encap/echoguard_linux.go:25-33`، `encap/obfs.go:86-91`).

---

## ۴. جدول ثابت‌ها، آستانه‌ها، اندازهٔ بافرها و زمان‌سنج‌ها

| نام | مقدار | path:line | معنی |
|---|---|---|---|
| `KindUDP/ICMP/GRE/IPIP/IPX` | `udp/icmp/gre/ipip/ipx` | `encap/encap.go:43-49` | نام کپسول‌ها |
| `DefaultIPXProto` | 253 | `encap/encap.go:80` | محدودهٔ آزمایشی RFC 3692 |
| `icmpOverhead` | 16 | `encap/raw.go:26` | ۸ بایت سرآیند echo و ۸ بایت nonce |
| `greOverhead` | 8 | `encap/raw.go:27` | flags، ptype، magic و id |
| `ipipOverhead` / `ipxOverhead` | 4 / 4 | `encap/raw.go:28-29` | magic و id |
| Overhead برای udp | 8 | `encap/encap.go:157` | سرآیند UDP |
| `sockBuf` | `4<<20` (۴ مگابایت) | `encap/udp.go:24` | بافر ارسال و دریافت همهٔ سوکت‌های حامل (UDP، خام، و فقط‌ارسال) |
| `rawPeerTTL` | 5 دقیقه | `encap/raw_linux.go:68` | پاک شدن همتای بی‌کار شنونده |
| `rawPeerMax` | 16384 | `encap/raw_linux.go:69` | سقف همتاها. در سرریز، sweep با TTL ده‌ثانیه‌ای و سپس **پاک کردن کل map** (`encap/raw_linux.go:1017-1021`) |
| `rawPeerSweep` | 1 دقیقه | `encap/raw_linux.go:70` | دورهٔ sweep |
| `rawMuxQueue` | 512 | `encap/raw_linux.go:163` | صف هر لینک پیش از `SetReceiver` (یعنی در دست‌دهی) |
| `rawBatch` | 32 | `encap/raw_linux.go:341` | datagram در هر `recvmmsg` یا `sendmmsg` |
| `rawBufSize` | 9216 | `encap/raw_linux.go:342` | بافر هر datagram در دریافت دسته‌ای (MTU تونل ۹۰۰۰ به‌اضافهٔ سرآیندها) |
| بافر خواندن تکی | 65536 | `encap/raw_linux.go:384`، `917` | مسیر بدون دسته‌ای |
| `bufPool` | ظرفیت 2048 | `encap/raw_linux.go:143` | scratch ساخت بسته |
| شناسهٔ لینک | ۱ تا ۶۵۵۳۵؛ سقف ۶۵۵۳۵ در حال استفاده | `encap/raw_linux.go:259-261` | «every link id ... is in use» |
| `camoIDBase` | `1+IntN(0xfffe)`، یعنی [1, 0xfffe] | `encap/raw_linux.go:37` | پایهٔ شبه‌PID، تصادفی برای هر پردازه |
| گام `camoLinkID` | `1+IntN(40)`، یعنی ۱ تا ۴۰ | `encap/raw_linux.go:44` | گام صعودی؛ `camoIDNext` در کل پردازه سراسری و فقط افزایشی است |
| `ipv4HdrLen` | 20 | `encap/rawtx_linux.go:64` | سرآیند ساخته‌شده در `rawTx` |
| `obfNonceLen` | 8 | `encap/obfs.go:51` | nonce روشن هر بسته |
| `obfSeqCounterBits` | 8 | `encap/obfs.go:59` | بایت پایین seq شمارنده است |
| `obfSeqCounterMask` / `obfSeqPrefixMask` | `0x00ff` / `0xff00` | `encap/obfs.go:60-61` | |
| `obfMaskLen` | 64 | `encap/obfs.go:68` | فقط ۶۴ بایت اول payload ماسک می‌شود (سرآیند ساختاری حامل حدود ۲۶ بایت است) |
| بافر `obfNoncer` | ۵۱۲ nonce (۴۰۹۶ بایت) | `encap/obfs.go:135` | هر ۵۱۲ بسته یک فراخوانی `crypto/rand` |
| پیشوند کمینه | `0x0100` | `encap/obfs.go:91` | جایگزین پیشوند صفر |
| `icmpEchoReply` / `icmpEchoRequest` | 0 / 8 | `encap/rawframe.go:48-49` | |
| `greFlagsKey` / `grePtypeIPv4` | `0x2000` / `0x0800` | `encap/rawframe.go:50-51` | بیت K و نسخهٔ ۰ |
| `bpfAccept` | `0x40000` | `encap/rawframe.go:269` | طول پذیرش BPF |
| DF درخواست / پاسخ | `IP_PMTUDISC_DO` / `IP_PMTUDISC_DONT` | `encap/raw_linux.go:127-130` | DF=1 روی درخواست، DF=0 روی پاسخ |
| حذف iptables | حداکثر ۸ نسخه | `encap/echoguard_linux.go:209` | |
| `echoMarkDir` | `/run/hs2/icmp-echo-ignore` | `encap/echoguard_linux.go:314` | نشانگرهای `<pid>.<netns>` |
| `echoIgnorePath` | `/proc/sys/net/ipv4/icmp_echo_ignore_all` | `encap/raw_linux.go:1131` | |
| **`icmpCamo` (CA1)** | `HS2_ICMP_CAMO=="1"` | `udpcarrier/carrier.go:35` | |
| **`camoIdlePoll` (CA1b)** | 700ms | `udpcarrier/carrier.go:41` | پایهٔ ضربان بی‌کاری |
| jitter در حالت فعال | `camoJitter(100ms, 0.4)`، یعنی ۶۰ تا ۱۴۰ میلی‌ثانیه | `udpcarrier/carrier.go:557,576` | |
| jitter در حالت بی‌کار (CA1b) | `camoJitter(700ms, 0.2)`، یعنی ۵۶۰ تا ۸۴۰ میلی‌ثانیه | `udpcarrier/carrier.go:579` | بیشینه ۸۴۰ میلی‌ثانیه، کمتر از `dgMuteAfter = 1s` |
| (CA1 پیشین) نمونه‌برداری بی‌کاری | `camoJitter(700ms, 0.5)`، یعنی ۳۵۰ تا ۱۰۵۰ میلی‌ثانیه، **بدون ارسال** | diff ثبت `da621f5` | جایگزین شد |
| `feedbackEvery` | 100ms | `udpcarrier/carrier.go:55` | ضربان بدون camo |
| `deadAfter` | 15s | `udpcarrier/carrier.go:54` | `ReadFrame` به `errDeadLink` می‌رسد |
| `flushEvery` / `expireEvery` | 10ms / 50ms | `udpcarrier/carrier.go:56-57` | |
| `baseProbeEvery` | 4s | `udpcarrier/rate.go:257` | کاوش پایهٔ مشترک |
| `camoProbeJit` | 1500ms | `udpcarrier/rate.go:369` | آفست ±۱٫۵ ثانیه، پس فاصلهٔ کاوش‌ها ۱ تا ۷ ثانیه |
| `pacerBatch` | 16 | `udpcarrier/pacer.go:87` | datagram در هر نوشتن دسته‌ای pacer |
| `CarrierOverhead` | 56 | `udpcarrier/mtu.go:10` | |
| `PathMTU` | 1500 | `udpcarrier/mtu.go:13` | |
| `InnerMTUFor(icmp,1500)` | 1408 (udp و gre: 1416؛ ipip و ipx: 1420) | `udpcarrier/mtu.go:18-31` | |
| MTU پیش‌فرض dgtun/udp/auto | 1280 | `cmd/hs2/main.go:367-369,831-834` | بستهٔ بیرونی ICMP در اندازهٔ کامل: 20+16+56+1280 = 1372 بایت |
| دست‌دهی | ۱۲ تلاش، `300+150×i` میلی‌ثانیه، ۱۰ ثانیه | `udpcarrier/dial.go:190-198` | |
| `confirmResend` | 150ms | `udpcarrier/dial.go:227` | تأیید ۶ ثانیه (کلاینت) و ۳ ثانیه (مهلت سرور) |
| `dgMuteAfter` | 1s | `engine/dgpool.go:74` | آستانهٔ «لال» بودن حامل |
| `dgMuteEvery` / `dgMuteHold` | 250ms / 500ms | `engine/dgpool.go:79-80` | |
| `dgSilentDead` | 3s | `engine/dgpool.go:61` | بسته شدن حامل لال |
| `dgPeerMuteFor` | 4s | `engine/dgpool.go:85` | اعتبار `closeMute` همتا |
| `dgPoolCtlEvery` | 3s | `engine/dgpool.go:56` | `publishTarget` لبهٔ معکوس، **روی یک حامل** |
| `dgInfoEvery` | 15s با jitter ۰٫۸ تا ۱٫۲ | `engine/dgpool.go:89`، `2080` | `publishInfo` لبهٔ مستقیم، روی یک حامل |
| `echoFillerPad` | 96 بایت؛ اندازهٔ پرکننده ۰ تا ۹۵ | `engine/dgpool.go:880,924` | |
| `icmpMaxLinks` | 8 | `cmd/hs2/main.go:678` | سقف حامل‌های tun روی icmp (اگر `max_links` صفر یا غایب باشد) |
| `keepaliveEvery` | 5s | `engine/engine.go:29` | فقط در موتور تک‌حاملی (udp/auto/noise)، **نه در dgtun** (مشاهدهٔ ۱۳.۲) |

---

## ۵. حلقه‌های کنترلی

| حلقه | ورودی | شرط | خروجی | دوره |
|---|---|---|---|---|
| `rawMux.readLoop` (`encap/raw_linux.go:349-396`) | `recvmmsg` تا ۳۲ بسته | `trunc` دور ریخته می‌شود؛ خطای نرم (`softErr`) یعنی ادامه | `deliver` به لینک؛ خطای سخت `fail` را صدا می‌زند: `mx.err`، بستن `dead` و `onErr` برای هر لینک در goroutine جدا | رویدادمحور |
| `rawPacketConn.ReadBatch` (`encap/raw_linux.go:866-912`) | `recvmmsg` | خطای نرم یعنی ادامه | `fn(payload, peer)` و بازگشت پس از یک دسته | با حلقهٔ `Listener.serve` |
| `notePeer` sweep (`encap/raw_linux.go:1036-1039`) | هر بستهٔ دریافتی | گذشتن `rawPeerSweep` از sweep قبلی | حذف همتاهای بیش از `rawPeerTTL` بی‌کار | ≥۱ دقیقه (در سرریز، فوری با TTL ده‌ثانیه‌ای) |
| `obfNoncer.next` (`encap/obfs.go:148-157`) | هر بسته | تمام شدن بافر | `crypto/rand` برای ۴۰۹۶ بایت | هر ۵۱۲ بسته |
| `rawTx.sendAll` (`encap/rawtx_linux.go:222-277`) | دستهٔ بسته‌ها | دستهٔ خطا | ادامه، fallback، یا خاموش شدن دائم | هر ارسال |
| `feedbackLoop` بدون camo (`udpcarrier/carrier.go:539-550`) | ticker | — | `sendFeedback` | ثابت ۱۰۰ میلی‌ثانیه (خط طیفی تیز ۱۰ هرتز) |
| `feedbackLoop` با camo (CA1b) (`udpcarrier/carrier.go:556-582`) | timer | `active := wireBytes != lastRx` (آیا از آخرین تیک **datagram داده** دریافت شده؟) | **همیشه** `sendFeedback`؛ اگر فعال باشد بعدی در ۶۰ تا ۱۴۰ میلی‌ثانیه، وگرنه در ۵۶۰ تا ۸۴۰ میلی‌ثانیه | jitterدار |
| `nextProbe` (`udpcarrier/rate.go:345-365`) | `now` | `r.fair` و `icmpCamo` | `epoch + (k+1)×4s + camoProbeOffset(k+1)` | ۴ ثانیه ±۱٫۵ ثانیه (قطعی برای k؛ همهٔ حامل‌های یک پردازه هم‌گام) |
| `muteLoop`/`muteTick` (`engine/dgpool.go:1047-1140`) | `LastRx` هر حامل | `stallGate`: اگر تیک بیش از دو برابر دوره دیر برسد، ۵۰۰ میلی‌ثانیه داوری نمی‌شود. `age<1s` یعنی «می‌شنود». اگر هیچ حاملی نشنود، همهٔ نشان‌ها برداشته می‌شوند | `muted=true` و دو بار `closeMute`؛ با شنیدن دوباره `closeHear`؛ در `age≥3s` بستن و جایگزینی | ۲۵۰ میلی‌ثانیه |
| `echoBalance` (`engine/dgpool.go:912-925`) | هر قاب **واقعی** دریافتی (Ping/Pong شمرده نمی‌شود: `engine/dgpool.go:775`) | `txFrames < rxFrames` | یک قاب پرکنندهٔ Ping (سمت شماره‌گیر) یا Pong (سمت شنونده) | به ازای هر قاب |
| `installEchoGuard` sweep (`encap/echoguard_linux.go:112-116`) | نخستین نصب در پردازه | مالک PID دیگر daemon زندهٔ hs2 نباشد | حذف جدول nft، قاعدهٔ iptables و نشانگر global | یک بار در هر پردازه، و نیز با `hs2 cleanup` |

---

## ۶. حالت‌ها، گذارها، خطاها و بازیابی

### ۶.۱ `rawTx` (سوکت فقط‌ارسال)
حالت‌ها: `nil` (مسیر قدیمی از ابتدا)، `ok`، `ok+noProto`، و `broken`.
- **`nil` از ابتدا** در این موارد: `HS2_RAW_TX=0` یا `HS2_RAW_BATCH=0`، `mmsg` پشتیبانی نشود، نوع غیر ICMP باشد، `IP_MTU_DISCOVER` برابر WANT باشد، یا باز کردن سوکت شکست بخورد (`encap/rawtx_linux.go:80-122`).
- `EINVAL` وقتی `withProto` روشن است (هستهٔ پیش از ۶٫۴ `IP_PROTOCOL` را نمی‌شناسد): `noProto=true` و **همان datagram** بدون آن دوباره فرستاده می‌شود. هر ارسال جداگانه این را می‌فهمد (رفع X7) (`encap/rawtx_linux.go:248-253`).
- `EMSGSIZE` بدون DF (پاسخ بزرگ): از مسیر قدیمی می‌رود که آن را قطعه‌قطعه می‌کند (`txFellBack++`).
- `softErr`: datagram از دست می‌رود و `sendRefused++`.
- `txSocketErr` (EBADF، ENOTSOCK، EFAULT، EOPNOTSUPP، EAFNOSUPPORT، EPROTONOSUPPORT، EDESTADDRREQ، ENOTCONN، یا خطای غیر Errno): `broken=true` **برای همیشه** و یک بار لاگ می‌شود؛ بقیهٔ بسته‌ها از مسیر قدیمی می‌روند.
- هر خطای دیگر (مثلاً `EPERM` دیوارهٔ آتش): فقط همان datagram از مسیر قدیمی می‌رود که آن را می‌فرستد یا همان خطا را گزارش می‌کند.
- `net.ErrClosed`: بازگشت.
- با `IP_RECVERR`، ریزش صف دستگاه هم به‌صورت `ENOBUFS` گزارش و در `sendRefused` شمرده می‌شود (`encap/rawtx_linux.go:40-47,127-129`).

### ۶.۲ `rawMux` و `rawConn`
- `rawMux` ابتدا زنده است. در خطای سخت خواندن به حالت `dead` می‌رود (`err` تنظیم و `dead` بسته می‌شود، و `onErr` هر لینک صدا زده می‌شود که حامل را می‌بندد). شماره‌گیری بعدی mux تازه باز می‌کند. mux خراب با آخرین لینکش بسته می‌شود و `releaseLocked` فقط اگر هنوز همان mux ثبت‌شده باشد آن را از map حذف می‌کند (`encap/raw_linux.go:262-268,444-452`).
- `rawConn` با `Close` بسته می‌شود: `done` بسته، شناسه آزاد و صف تخلیه می‌شود. `Read` در این حالت `net.ErrClosed` و `Write` هم `net.ErrClosed` برمی‌گرداند.
- `Read` با `rdl` و `rdlSet` مهلت را حتی وقتی خواندن مسدود است اعمال می‌کند (`encap/raw_linux.go:454-494,706-717`). `SetWriteDeadline` کاری نمی‌کند (`encap/raw_linux.go:718`).

### ۶.۳ دسته‌بندی خطاها
- **`softErr`** (`encap/raw_linux.go:1209-1216`): EHOSTUNREACH، ECONNREFUSED، ENETUNREACH، ECONNRESET، EHOSTDOWN، ENETDOWN، EMSGSIZE، ENOBUFS، ENOPROTOOPT، ENONET، EPROTO. در خواندن یعنی ادامه، و در نوشتن یعنی دور ریختن همان datagram و `sendRefused++`.
- **سوکت شماره‌گیر بدون اتصال** است، پس یک ICMP خطا (حتی جعلی) لینک را از بین نمی‌برد (`encap/raw_linux.go:311-318`). آزمون: `TestRawDialSurvivesICMPError`.
- **`rawErr`**: EPERM یا EACCES به پیام «needs root or CAP_NET_RAW» تبدیل می‌شوند (`encap/raw_linux.go:1218-1223`).

### ۶.۴ echoguard
- `guards[m]` ابتدا وجود ندارد. نخستین acquire آن را با `{refs=1, method}` نصب می‌کند، acquireهای بعدی `refs++` می‌کنند، و release آخر قاعده را برمی‌دارد. `Close` دوباره با `closeEcho sync.Once` بی‌اثر است.
- خرابی (SIGKILL، OOM، قطع برق) قاعده‌ای با PID مرده باقی می‌گذارد که این‌ها برش می‌دارند: نخستین نصب در daemon بعدی، `hs2 cleanup`، یا برای global `sweepEchoIgnoreMarkers`. قاعدهٔ بی‌PID (از باینری قدیمی) فقط با `legacy=true` حذف می‌شود، یعنی وقتی `olderHs2Running()` نادرست باشد (`cmd/hs2/cleanup.go:21,37-57`).
- اگر نصب شکست بخورد، `Listen` با خطا برمی‌گردد و سوکتی باز نمی‌شود (`encap/raw_linux.go:810-812`).

### ۶.۵ حامل لال در engine
حامل از حالت «می‌شنود» به «لال» می‌رود اگر خودش ≥۱ ثانیه چیزی نشنیده باشد در حالی که حامل دیگری می‌شنود. حامل لال یا دوباره می‌شنود (و `closeHear` می‌فرستد) یا در ≥۳ ثانیه بسته و جایگزین می‌شود. گرفتن `closeMute` از همتا باعث `avoid` به مدت ۴ ثانیه می‌شود (`engine/dgpool.go:303-309,861-868`). **CA1 و CA1b مستقیماً با این ماشین حالت تداخل دارند** (§۱۲ و §۱۳).

---

## ۷. قالب قاب‌ها و پیام‌ها

### ۷.۱ ICMP (روی سیم)
```
IPv4 (20B): TTL/TOS از سوکت دریافت؛ DF=1 روی درخواست (id=0)، DF=0 روی پاسخ (id را هسته انتخاب می‌کند)؛ proto=1
ICMP:
  [0] type   : 8 (dial→listen، echo request) | 0 (listen→dial، echo reply)
  [1] code   : 0
  [2:4] csum : چک‌سام اینترنت روی کل ICMP (پس از ماسک)
  [4:6] id   : شناسهٔ لینک (روشن؛ شبه‌PID با HS2_ICMP_CAMO=1)
  [6:8] seq  : [بایت پیشوند جهت (کلیددار، ثابت در طول عمر)] [شمارندهٔ ۸بیتی]
  [8:16]     : nonce هشت‌بایتی روشن (برای هر بسته)
  [16:16+min(64,len)] : payload با XOR ChaCha20(obfKey, 0^4||nonce)
  [...]      : باقی payload (متن رمز AEAD) بدون تغییر
```
(`encap/rawframe.go:157-169`، `encap/obfs.go:12-44`، `encap/rawtx_linux.go:30-38`)
- شمارندهٔ درخواست برای هر لینک است، از مقدار تصادفی شروع می‌شود و با هر بسته یکی بالا می‌رود (`encap/raw_linux.go:286-288,504`).
- شمارندهٔ پاسخ برای هر (IP، id) و مستقل از درخواست است و با بذر تصادفی شروع می‌شود (`encap/raw_linux.go:743-748,1024-1029,1062-1071`).
- `parse` در ICMP فقط این‌ها را بررسی می‌کند: نوع، code=0، و بایت پیشوند. checksum، id، بایت شمارنده و nonce بررسی نمی‌شوند (`encap/rawframe.go:190-200`؛ آزمون `TestRawFrameRejectsGarbage`).

### ۷.۲ GRE (پروتکل ۴۷)
`[0x2000 flags/ver][0x0800 ptype][magic:2][id:2] payload` (`encap/rawframe.go:170-174`). کلید GRE همان `magic||id` است.

### ۷.۳ IPIP (پروتکل ۴) و IPX (پروتکل انتخابی، پیش‌فرض ۲۵۳)
`[magic:2][id:2] payload` (`encap/rawframe.go:175-177`).

### ۷.۴ payload (یکسان در همهٔ کپسول‌ها): datagram حامل
`[tag:1]`، که یکی از این‌هاست:
- `0x00` داده: `[wireSeq:4]` و سپس shard اف‌ای‌سی؛
- `0x01` کنترلی: یک قاب کنترلی مهرشده؛
- `0x02` داده با مهر زمانی: `[wireSeq:4][sendStamp:4]` و سپس shard اف‌ای‌سی.

(`udpcarrier/carrier.go:205-209`). این دقیقاً همان چیزی است که ماسک ۶۴بایتی ICMP پنهان می‌کند.

### ۷.۵ فیلتر BPF (`encap/rawframe.go:281-317`)
`LDX MSH [0]` (X = طول سرآیند IP)، سپس برای هر شرط یک جفت `LD` و `JEQ`، سپس `RET 0x40000` (پذیرش) یا `RET 0` (رد).
- سمت شماره‌گیر: `LD W ABS [12] == srcIP` (منبع باید همتا باشد).
- ICMP: `LDB [X+0] == rxType` و `LDB [X+6] == rxPrefix>>8`.
- GRE: `LDH [X+0] == 0x2000` و `LDH [X+4] == rxMagic`.
- IPIP/IPX: `LDH [X+0] == rxMagic`.
- فیلتر شناسهٔ لینک وجود دارد ولی سوکت مشترک با `id=0` آن را خاموش نگه می‌دارد و demux را در فضای کاربر انجام می‌دهد (`encap/raw_linux.go:327`).
- شنونده: فقط نوع و پیشوند (یا magic) بررسی می‌شود و منبع بررسی نمی‌شود (`recvFilter(0,0)`، `encap/raw_linux.go:821`).

### ۷.۶ قاب‌های کنترلی engine مرتبط
- `TypeClose` با op برابر `closeMute` یا `closeHear` (`engine/dgpool.go:861-868`).
- `TypePing`/`TypePong` در dgtun فقط **پرکنندهٔ echo** هستند و پاسخ داده نمی‌شوند (`engine/dgpool.go:797-800`).

### ۷.۷ اشتقاق کلیدها از shared secret
- magic: `HMAC-SHA256(secret, "hs2-encap-magic-v1\0"+kind+"\0"+proto+"\0"+dir)` (`encap/rawframe.go:134-145`)؛
- کلید ماسک ICMP: `BLAKE2s-256(key=secret[:32], "hs2-icmp-obfs-key-v1")` (`encap/obfs.go:74-78`)؛
- پیشوندها: `BLAKE2s-256(key=secret[:32], "hs2-icmp-obfs-seq-v1\0"+dir)[0] & 0xff00` (`encap/obfs.go:85-109`).

---

## ۸. متن دقیق لاگ‌ها و پیام‌های خطای مهم

| متن | path:line | معنی |
|---|---|---|
| `encap icmp: carriers send through a send-only raw socket, without the shared socket's lock (HS2_RAW_TX=0 turns it off)` | `encap/rawtx_linux.go:153` | یک بار در هر پردازه، وقتی نخستین `rawTx` باز شود |
| `encap icmp: the send-only raw socket failed (%v) — its carriers send on the shared socket from now on` | `encap/rawtx_linux.go:263` | `rawTx` برای همیشه به مسیر قدیمی رفت |
| `encap icmp: removed a stale reply rule left by a stopped daemon: %s` | `encap/echoguard_linux.go:114` | sweep هنگام نخستین نصب |
| `encap icmp: neither nft nor iptables worked (%s) — turned off ALL ping replies on this server while the tunnel runs (net.ipv4.icmp_echo_ignore_all=1); install nftables to keep normal ping working` | `encap/echoguard_linux.go:144` | افتادن به حالت global (اگر اجباری نبوده باشد) |
| `encap icmp: cannot stop the kernel answering the tunnel's echo requests (%s)` | `encap/echoguard_linux.go:141,149` | خطای Listen: هیچ روشی کار نکرد |
| `encap icmp: the kernel would answer the tunnel's echo requests itself; set net.ipv4.icmp_echo_ignore_all=1 (sysctl -w) or run as root: %w` | `encap/raw_linux.go:1158-1159` | نوشتن sysctl شکست خورد |
| `encap %s: a raw socket needs root or CAP_NET_RAW: %w` | `encap/raw_linux.go:1220` | نبود دسترسی |
| `encap %s: every link id to %s is in use` | `encap/raw_linux.go:260` | ۶۵۵۳۵ شناسه در حال استفاده است |
| `encap %s: attach receive filter: %w` | `encap/raw_linux.go:116,329` | نصب BPF شکست خورد |
| `encap ipx: IP protocol %d is not usable for ipx (1..254, and not one the kernel handles)` | `encap/rawframe.go:108` | |
| `encap: raw-socket encapsulations require Linux` | `encap/raw.go:34` | |
| `encap: unknown kind %q` | `encap/encap.go:126,146` | |
| `udpcarrier: no handshake reply from %s (the path drops it, the other server is not running, or its shared_key differs)` | `udpcarrier/dial.go:222` | پایان دست‌دهی بی‌پاسخ |
| `dg: carrier %d has heard nothing from the other server for %.1fs while %d other carrier(s) still do — its own way through is cut: new flows avoid it and %s` | `engine/dgpool.go:1131` | حامل لال شد (با CA1 به‌شکل flapping دیده می‌شد) |
| `dg: carrier %d hears the other server again — it takes flows again` | `engine/dgpool.go:1112` | برداشته شدن حالت لال |
| `dg: carrier %d heard nothing for %.1fs — closed; a new carrier replaces it` | `engine/dgpool.go:1138` | بسته شدن در ۳ ثانیه |
| `dg: carrier %d: the other server hears nothing on it — what goes there is lost: its flows move to live carriers` | `engine/dgpool.go:863` | گرفتن `closeMute` |
| `tun %s up: %s peer %s mtu %d (datagram pool, encap %s, %s)` | `cmd/hs2/main.go:851` | آغاز dgtun |
| `encap icmp: neither nft nor iptables is installed, so ... must turn off ALL ping replies ...` | `cmd/hs2/check.go:309` | هشدار `hs2 check` روی شنوندهٔ ICMP |

خطوط `muteLog` با `burstLog` جمع‌بندی می‌شوند (`engine/dgpool.go:608`، `engine/burstlog.go:35-62`).

---

## ۹. گزینه‌های پیکربندی و متغیرهای محیطی

### ۹.۱ فیلدهای پیکربندی (`cmd/hs2/main.go`)
| فیلد | اثر |
|---|---|
| `"carrier": "dgtun"` | تنها حاملی که کپسول را انتخاب‌پذیر می‌کند (`main.go:420-421`) |
| `"encap": "udp\|icmp\|gre\|ipip\|ipx"` | `main.go:50`. رشتهٔ تهی یعنی udp. در `hs2 check` به بزرگی و کوچکی حروف حساس است (`check.go:90-92,298`) |
| `"proto"` | فقط برای ipx (`main.go:51`؛ اعتبارسنجی در `check.go:301-307`) |
| `"addr"` | برای کپسول خام فقط IP لازم است و پورت نادیده گرفته می‌شود (`check.go:151-169`؛ `encap/encap.go:202-207`) |
| `"bind_local_ip"` | به `EncapConfig.BindIP` و سپس `Options.BindIP` می‌رسد (`main.go:853`) |
| `"max_links"` | روی icmp اگر صفر یا غایب باشد سقف ۸ است (`main.go:671-683`؛ doctor در `doctor.go:355-364`) |
| `"mtu"` | پیش‌فرض ۱۲۸۰ برای udp/auto/dgtun (`main.go:367-369`) |
| `"reverse"` | تعیین می‌کند کدام سمت شماره بگیرد، و بنابراین کدام سمت درخواست ICMP بفرستد و echoguard کجا نصب شود (`main.go:907`) |

### ۹.۲ متغیرهای محیطی
| متغیر | path:line | اثر | زمان خواندن |
|---|---|---|---|
| `HS2_ICMP_CAMO=1` | `encap/raw_linux.go:29` (CA2) و `udpcarrier/carrier.go:35` (CA1 و CA1b و کاوش) | شناسهٔ شبه‌PID (فقط ICMP) به‌اضافهٔ زمان‌بندی jitterدار feedback و کاوش (**برای همهٔ حامل‌های udpcarrier پردازه، صرف‌نظر از kind**؛ مشاهدهٔ ۱۳.۴) | یک بار در init بسته؛ نیاز به راه‌اندازی دوباره دارد |
| `HS2_ICMP_SUPPRESS=nft\|iptables\|global` | `encap/echoguard_linux.go:117-122` | اجبار روش echoguard | هنگام نخستین نصب هر بایت پیشوند |
| `HS2_RAW_BATCH=0` | `encap/raw_linux.go:400` | یک datagram در هر syscall روی سوکت خام؛ **`rawTx` را هم خاموش می‌کند** (`encap/rawtx_linux.go:68`) | init |
| `HS2_RAW_TX=0` | `encap/rawtx_linux.go:68` | خاموش کردن سوکت فقط‌ارسال (و طبق README، `SendNoLock` شنوندهٔ UDP) | init |
| `HS2_DG_PAD=0` | `udpcarrier/carrier.go:20` | خاموش کردن padding سطلی (B6) برای همهٔ کپسول‌ها | init |
| `HS2_ENCAP_NETNS` | `encap/netns_linux_test.go:420` | فقط برای آزمون (اجرای دوباره در netns خصوصی) | — |

### ۹.۳ وابستگی‌های سیستمی
- `CAP_NET_RAW` (یعنی root؛ یونیت systemd بدون محدودیت capability است: `install.sh:816-823`)؛
- `nft` یا `iptables` با ماژول u32 (نصب‌کننده `nftables` و `iputils-ping` را نصب می‌کند: `install.sh:283-287`)؛
- sysctl `net.ipv4.icmp_echo_ignore_all` (فقط در حالت global)؛
- هستهٔ ≥۶٫۴ برای `IP_PROTOCOL` (اختیاری).

---

## ۱۰. آزمون‌ها: چه چیزی تضمین می‌شود

### ۱۰.۱ `encap` (اجرا شد: همه سبز، ۳ مورد رد شد)
| آزمون | فایل | تضمین |
|---|---|---|
| `TestCamoLinkIDClusters` | `camoid_test.go:8` | ۸ شناسه یکتا، ناصفر، صعودی، با گام ۱ تا ۴۰ و پراکندگی کل ≤۳۲۰ |
| `TestCamoLinkIDWrapsSafely` | `camoid_test.go:38` | نزدیک مرز `0xfff0`، پنجاه شناسه یکتا و ناصفر می‌مانند |
| `TestObfKeyFromSecret` / `TestObfSeqPrefixes` / `TestObfSeqBuildAndMatch` | `obfs_test.go` | کلید ۳۲ بایتی و قطعی است؛ پیشوندها متفاوت، ناصفر، فقط در ۸ بیت بالا، پایدار و وابسته به secret هستند؛ شمارنده حفظ می‌شود |
| `TestObfMaskInverse` / `TestObfMaskKeyAndNonceSeparate` / `TestObfMaskErasesCounterStructure` | `obfs_test.go` | ماسک وارون خودش است؛ nonce و کلید جدا هستند؛ در هر موقعیت بایت دست‌کم ۲۰۰ مقدار از ۲۵۶ در ۴۰۹۶ بسته دیده می‌شود |
| `TestRawFrameRoundTrip` / `ICMPWellFormed` / `GREWellFormed` / `KeySeparation` / `RejectsGarbage` / `TestIPXProto` / `TestIPv4Payload` / `TestRecvFilter` | `rawframe_test.go` | سرآیند دقیقاً `Overhead` بایت است؛ هیچ سمتی جهت خودش را نمی‌پذیرد؛ ICMP خوش‌ساخت است (نوع و checksum)؛ کلید متفاوت رد می‌شود (با برخورد ۱ در ۲۵۶ برای ICMP)؛ پروتکل‌های ممنوع ipx رد می‌شوند؛ IHL و قطعه‌ها درست رفتار می‌شوند؛ منطق BPF با مفسر داخلی، شامل فیلتر منبع و گزینه‌های IP، درست است |
| `TestRawSocketRoundTrip` / `LinksDemux` / `ForeignKeyIgnored` / `WildcardRepliesFromTarget` / `BindIP` | `raw_linux_test.go` | سوکت واقعی برای هر ۴ نوع: اندازه‌های ۰ تا ۱۴۷۲؛ demux بر پایهٔ شناسه؛ کلید بیگانه هرگز نمی‌رسد؛ wildcard از نشانی هدف پاسخ می‌دهد |
| `TestRawSocketICMPKernelSilent` | `raw_linux_test.go:289` | با sniffer: هسته به درخواست‌های تونل **هیچ** پاسخ echo نمی‌دهد |
| `TestRawSocketICMPReplySequence` / `ReplyCounterUnique` (B4) | `raw_linux_test.go:340,369` | پاسخ پیشوند s2c دارد؛ شمارنده ۱+ است؛ ۳۰۰ پاسخ هیچ (id,seq) تکراری ندارند |
| `TestICMPEchoGuardKeepsHostPing` (رد شد: `ping` نصب نیست) | `echoguard_linux_test.go:67` | با nft یا iptables، پینگ عادی پاسخ می‌گیرد، تونل پاسخ هسته ندارد، و قاعده با Close برداشته می‌شود |
| `TestICMPEchoGuardRefcount` / `TestSweepStaleEchoGuards` / `TestSweepRestoresGlobalEchoIgnore` | `echoguard_linux_test.go` | شمارش ارجاع؛ sweep فقط مالک مرده و قاعدهٔ legacy را حذف می‌کند، نه daemon زنده و نه جدول بیگانه؛ global فقط وقتی برمی‌گردد که daemon زنده‌ای نمانده باشد |
| `TestICMPEchoRestoredOnClose` / `TestICMPEchoLeftAloneWhenPreSet` | `echoignore_linux_test.go` | حالت global شمارش ارجاع دارد و مقدار تنظیم‌شده توسط مدیر سرور را دست نمی‌زند |
| `TestRawDialSurvivesICMPError` / `TestRawDialSourceFiltered` | `icmperr_linux_test.go` | ICMP خطا (کدهای ۱، ۲، ۳، ۴ و ۱۳) لینک را نمی‌کشد؛ بستهٔ جعلی از منبع دیگر پذیرفته نمی‌شود |
| `TestRawMuxSharesOneSocket` / `StalledLinkDoesNotBlockOthers` / `CloseAndDeadline` | `rawmux_linux_test.go` | ۳۰۰ لینک یک سوکت دارند؛ شناسه‌ها یکتا هستند؛ لینک گیرکرده فقط بستهٔ خودش را از دست می‌دهد؛ مهلت و Close خواندن مسدود را بیدار می‌کنند |
| `TestRawBatchRoundTrip` / `TestRawBatchSoftErrorDropsOnlyThatDatagram` (رد شد: `ip` نیست) | `batch_linux_test.go` | دستهٔ ۱۰۰تایی کامل و به‌ترتیب می‌رسد؛ خطای نرم فقط همان datagram را از دست می‌دهد |
| `TestRawTxHeaderMatchesKernel` / `Oversize` / `ReceivesNothing` / `BrokenFallsBack` / `ConcurrentFullBuffer` / `ProtoRefusedConcurrently` / `FirewallRefusalIsPerDatagram` / `QueueDropsCounted` (رد شد: `tc` نیست) | `rawtx_linux_test.go` | سرآیند ساخته‌شده با سرآیند هسته برابر است (DF و id)؛ پاسخ بزرگ قطعه‌قطعه می‌شود و درخواست بزرگ شمرده و دور ریخته می‌شود؛ سوکت فقط‌ارسال چیزی دریافت نمی‌کند؛ fallback درست است؛ با بافر پر چیزی از دست نمی‌رود؛ `EINVAL` سوکت را خاموش نمی‌کند؛ `EPERM` برای هر datagram جداست؛ ریزش صف شمرده می‌شود |
| `TestUDPSockBufs` / `TestUDPSockBufAbsorbsReaderPause` | `sockbuf_linux_test.go` | بافر ≥۴ مگابایت است؛ ۱۵۰۰ datagram در زمان مکث خواننده از دست نمی‌روند |
| `TestCarrierOverRawEncap` / `RawManyLinks` (۴۸) / `RawWrongKey` / `TestRawFramingMagicKeyedBySecret` / `TestCarrierRawOnWireMTU` | `carrier_raw_linux_test.go` | حامل کامل روی هر ۴ نوع با قاب‌های MTU کامل؛ اندازهٔ IPv4 روی سیم دقیقاً `20+Overhead+56+innerMTU` و ≤۱۵۰۰ است |
| `TestOnWireCiphertextOnly` / `TestSealedDatagramTamperTotal` | `crypto_linux_test.go` | متن روشن روی سیم دیده نمی‌شود؛ تغییر هر بایت متن رمز، باز شدن را ناکام می‌کند |
| `BenchmarkObfMask` / `BenchmarkFramerBuildICMP` / `GRE` | `bench_test.go` | سنجش هزینه (تضمین رفتاری نیست) |

### ۱۰.۲ بیرون از encap
- **udpcarrier** (`udpcarrier/camo_test.go`):
  - `TestCamoProbeOffsetDeterministicBounded`: آفست قطعی است، در ±۱٫۵ ثانیه می‌ماند، و ≥۱۵۰۰ مقدار متمایز از ۲۰۰۰ دارد؛
  - `TestCamoProbeSynchronisedButNotFixedGrid`: دو حامل زمان یکسانی دارند، شبکهٔ زمانی ثابت نیست، و فاصله‌ها در `4s±3s` هستند؛
  - `TestCamoJitterBounds`: بازهٔ ۶۰ تا ۱۴۰ میلی‌ثانیه با میانگین حدود ۱۰۰ میلی‌ثانیه.
  - **آزمونی برای CA1b (ضربان ۰٫۷ ثانیه‌ای یا نبود flapping) وجود ندارد** (مشاهدهٔ ۱۳.۱۸).
- **udpcarrier** (`udpcarrier/encapkey_test.go:17`): `TestEncapFramingKeyedBySecret`، یعنی `Key` باید همان secret باشد.
- **udpcarrier** (`udpcarrier/mtu_test.go`): `TestCarrierOverhead` و `TestInnerMTUFor`.
- **engine** (`engine/dgecho_test.go`):
  - `TestCarrierEchoShaped`: فقط ICMP شکل داده می‌شود؛
  - `TestDgEchoReverseEdgeReplyPerRequest` و `TestDgEchoDirectEdgePullsRequests`: نسبت ۱:۱؛
  - `TestDgPoolICMPShapedBothModes`: دو balancer روبه‌رو طوفان پرکننده نمی‌سازند؛
  - `TestDgEchoHeavySideNoFiller`.
- **engine** (`engine/dgmute_test.go`): `TestDgMuteCarrierAvoidedThenClosed`، `TestDgMuteCarrierHearsAgain`، `TestDgMuteNotJudgedWhenEveryCarrierIsSilent`، `TestDgMuteToldToTheOtherSide`، `TestDgPeerMuteExpires`، `TestDgMuteClosedOnce` و `TestDgPoolCtlAvoidsMuteCarrier`.
- **cmd/hs2**: `TestICMPTunCeiling` (`cmd/hs2/linkpool_test.go:698`) و `TestDoctorICMPCeiling` (`758`).

---

## ۱۱. «از قبل وجود دارد» (برای جلوگیری از دوباره‌کاری)

**قالب سیم و مبهم‌سازی ICMP (همیشه روشن، از نسخه‌های پیشین):**
1. حذف magic ثابت در ICMP؛ پیشوند کلیددار جهت در بایت بالای seq و شمارندهٔ ۸بیتی در بایت پایین (`encap/obfs.go`).
2. nonce هشت‌بایتی برای هر بسته و ماسک ChaCha20 روی ۶۴ بایت اول payload، یعنی شمارنده‌ها و سرآیند FEC روی سیم دیده نمی‌شوند.
3. نوع درخواست و پاسخ درست، checksum درست، DF مثل ping (۱ روی درخواست و ۰ روی پاسخ) و id سرآیند IP هم مثل هسته (`TestRawTxHeaderMatchesKernel`).
4. شمارندهٔ پاسخ مستقل برای هر همتا با بذر تصادفی، بی‌تکرار و صعودی (B4 در توضیح آزمون: `raw_linux_test.go:337`).
5. seq درخواست با بذر تصادفی برای هر لینک.
6. magic کلیددار برای هر نوع، جهت و پروتکل در gre/ipip/ipx (HMAC).
7. شکل‌دهی echo نسبت ۱:۱ با پرکنندهٔ Ping/Pong در جهت سبک‌تر (B5، `engine/dgpool.go:896-925`)، هم در حالت مستقیم و هم معکوس.
8. padding سطلی اندازه برای قاب‌های کوچک و متوسط در همهٔ کپسول‌ها (B6، `udpcarrier/carrier.go:17-20,769-779`، با `HS2_DG_PAD=0` خاموش می‌شود).

**استتار اختیاری (فاز CA، پشت `HS2_ICMP_CAMO=1`):**
9. CA1: feedback با jitter در بازهٔ ۶۰ تا ۱۴۰ میلی‌ثانیه به‌جای تیک ثابت ۱۰۰ میلی‌ثانیه‌ای.
10. CA1b: ضربان بی‌کاری حدود ۰٫۷ ثانیه (۵۶۰ تا ۸۴۰ میلی‌ثانیه) به‌جای سکوت کامل.
11. CA1: jitter قطعی و مشترک کاوش پایه به اندازهٔ ±۱٫۵ ثانیه حول دورهٔ ۴ ثانیه، که هم‌گامی استخر را حفظ می‌کند.
12. CA2: شناسهٔ لینک شبه‌PID به شکل خوشه‌ای صعودی (گام ۱ تا ۴۰).

**سوکت و کارایی:**
13. یک سوکت دریافت مشترک برای هر همتا با demux در فضای کاربر (Q5)؛ شناسهٔ یکتا برای هر (kind، peer).
14. سوکت شماره‌گیر بدون اتصال، همراه با فیلتر منبع در BPF و در فضای کاربر (مقاوم در برابر ICMP خطا و جعل).
15. فیلتر BPF کلیددار در هسته (رد ارزان ترافیک غیرمرتبط).
16. `recvmmsg`/`sendmmsg` با دسته‌های ۳۲تایی (W1) و `SendNoLock`.
17. سوکت فقط‌ارسال `IPPROTO_RAW` برای ICMP همراه با `IP_RECVERR` و `IP_PROTOCOL` و fallback (X5 و X7).
18. بافر ۴ مگابایتی اجباری روی همهٔ سوکت‌ها.
19. شمارندهٔ `send_refused` در وضعیت (`cmd/hs2/status.go:312`).
20. noncer دسته‌ای (هیچ syscall برای هر بسته).

**echoguard و چرخهٔ عمر:**
21. قاعدهٔ nft، سپس iptables u32، سپس global؛ فقط پاسخ‌های دارای پیشوند c2s دور ریخته می‌شوند تا پینگ عادی کار کند.
22. نام‌گذاری قاعده با PID، sweep خودکار هنگام شروع، `hs2 cleanup`، نشانگر global وابسته به netns، و `ReleaseAllEchoGuards` در همهٔ مسیرهای خروج.
23. هشدار `hs2 check` وقتی nft یا iptables نیست (`check.go:308-310`)؛ و راهنمای `HS2_ICMP_SUPPRESS=global` برای صرفه‌جویی پردازنده روی سرور اختصاصی (CHANGELOG V3).

**سیاست استخر روی icmp:**
24. سقف ۸ حامل (V2).
25. آشکارساز حامل لال در ۱ ثانیه و بستن در ۳ ثانیه (V1).
26. صف منصفانه و خط سریع (V4؛ مختص icmp نیست).

---

## ۱۲. ایده‌هایی که امتحان یا بررسی و رد یا کنار گذاشته شده‌اند

| ایده | سرنوشت و دلیل | منبع |
|---|---|---|
| **CA1: سکوت کامل حامل بی‌کار** (با تکیه بر keepalive حدود ۵ ثانیه) | **پسرفت در میدان و جایگزینی با CA1b.** آشکارساز لال همتا (`dgMuteAfter = 1s`، که بر پایهٔ ضربان ۱۰۰ میلی‌ثانیه‌ای ساخته شده) روی سکوت فعال می‌شد. حدود ۳۹ رویداد mute در ۲٫۵ دقیقه رخ داد و تعداد لینک بین ۳ و ۵ نوسان می‌کرد. مرگبار نبود ولی معیار «بدون churn» را رد می‌کرد | پیام ثبت `da621f5`؛ `udpcarrier/carrier.go:27-30,567-573` |

> یادداشت: نگارش این فایل پیش از تکمیل بخش‌های ۱۲ تا ۱۴ متوقف شد؛ آن بخش‌ها در این نسخه موجود نیستند.
