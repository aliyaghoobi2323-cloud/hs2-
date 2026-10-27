### `N=1 RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=10 MIX=home DUR=10 SBR=1.2`

| | `old` (1) | `b2nx` (1) |
|---|---|---|
| **context** | | |
| all users dn / cap_P10 % | 97 | 97 |
| all users up / cap_P10 % | 51 | 55 |
| heavy user mean Mbit/s | 71.59 | 72.69 |
| heavy user min Mbit/s | 71.59 | 72.69 |
| **game TCP** | | |
| user to server p50 +ms | 2.8 | 0.2 |
| user to server p99 +ms | 59.4 | 1.6 |
| user to server p99.9 +ms | 110.7 | 5.6 |
| user to server jitter p99 ms | 15.8 | 2.7 |
| user to server late+lost % | 0.31 | 0.00 |
| user to server floor - RTT/2 ms | 0.19 | 0.15 |
| server to user p50 +ms | 209.0 | 0.2 |
| server to user p99 +ms | 396.6 | 111.8 |
| server to user p99.9 +ms | 452.9 | 143.1 |
| server to user jitter p99 ms | 128.8 | 65.5 |
| server to user late+lost % | 93.67 | 1.67 |
| server to user floor - RTT/2 ms | 4.25 | 0.16 |
| **game UDP** | | |
| user to server p50 +ms | 2.7 | 0.2 |
| user to server p99 +ms | 57.3 | 0.9 |
| user to server p99.9 +ms | 109.5 | 4.0 |
| user to server jitter p99 ms | 15.6 | 1.3 |
| user to server late+lost % | 0.31 | 0.00 |
| user to server floor - RTT/2 ms | 0.16 | 0.15 |
| **call** | | |
| user to server p50 +ms | 2.5 | 7.3 |
| user to server p99 +ms | 58.2 | 128.7 |
| user to server p99.9 +ms | 112.1 | 208.8 |
| user to server jitter p99 ms | 19.7 | 98.2 |
| user to server late+lost % | 0.00 | 0.70 |
| user to server floor - RTT/2 ms | 0.35 | 0.31 |
| user to server delivered % | 100.0 | 100.0 |
| server to user p50 +ms | 210.3 | 113.6 |
| server to user p99 +ms | 408.9 | 870.4 |
| server to user p99.9 +ms | 460.3 | 910.5 |
| server to user jitter p99 ms | 130.1 | 242.0 |
| server to user late+lost % | 82.00 | 48.60 |
| server to user floor - RTT/2 ms | 3.84 | 1.94 |
| server to user delivered % | 100.0 | 100.0 |
| **stream** | | |
| bitrate / fair share Mbit/s | 1.20 / 27.76 | 1.20 / 28.16 |
| startup p50 s | 0.60 | 0.18 |
| startup max s | 0.89 | 0.18 |
| not started | 0 | 0 |
| rebuffers (all viewers) | 0 | 0 |
| viewers with a rebuffer | 0 | 0 |
| rebuffer time s (all) | 0.0 | 0.0 |
| segment rate Mbit/s | 7.93 | 9.80 |
| **web** | | |
| page load p50 ms | 2042 | 1319 |
| page load p95 ms | 2142 | 1570 |
| first byte p50 ms | 230 | 75 |
| first byte p95 ms | 319 | 75 |
| pages (failed) | 4 (0) | 4 (0) |

