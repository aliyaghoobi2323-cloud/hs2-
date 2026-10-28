### `N=1 RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=50 MIX=gamer_vs_dl`

| | `old` (2) | `b2nx` (2) |
|---|---|---|
| **context** | | |
| all users dn / cap_P10 % | 16 (19, 13) | 13 (13, 14) |
| all users up / cap_P10 % | 1 (1, 1) | 1 (1, 1) |
| heavy user mean Mbit/s | 0.57 (0.67, 0.47) | 0.42 (0.41, 0.42) |
| heavy user min Mbit/s | 0.27 (0.19, 0.35) | 0.36 (0.36, 0.37) |
| **game TCP** | | |
| user to server p50 +ms | 0.4 (0.3, 0.5) | 0.3 (0.4, 0.3) |
| user to server p99 +ms | 1709.7 (942.0, 2477.4) | 1.4 (1.8, 1.0) |
| user to server p99.9 +ms | 2842.4 (2868.7, 2816.2) | 2.2 (2.6, 1.8) |
| user to server jitter p99 ms | 16.5 (16.4, 16.6) | 1.3 (1.7, 0.9) |
| user to server late+lost % | 15.92 (9.16, 22.67) | 0.00 (0.00, 0.00) |
| user to server floor - RTT/2 ms | 0.11 (0.10, 0.12) | 0.11 (0.11, 0.11) |
| server to user p50 +ms | 13291.6 (10458.3, 16124.9) | 4.8 (4.9, 4.8) |
| server to user p99 +ms | 20362.8 (16223.5, 24502.0) | 15.4 (17.0, 13.7) |
| server to user p99.9 +ms | 20693.1 (16587.3, 24798.8) | 19.5 (22.0, 17.1) |
| server to user jitter p99 ms | 410.8 (332.6, 489.1) | 12.9 (14.8, 11.0) |
| server to user late+lost % | 100.00 (100.00, 100.00) | 0.00 (0.00, 0.00) |
| server to user floor - RTT/2 ms | 1660.10 (1680.84, 1639.36) | 0.38 (0.37, 0.39) |
| **game UDP** | | |
| user to server p50 +ms | 0.3 (0.3, 0.4) | 0.3 (0.4, 0.3) |
| user to server p99 +ms | 128.4 (128.3, 128.4) | 1.4 (1.8, 1.0) |
| user to server p99.9 +ms | 225.9 (225.0, 226.7) | 2.5 (2.7, 2.2) |
| user to server jitter p99 ms | 14.6 (14.7, 14.6) | 1.2 (1.6, 0.9) |
| user to server late+lost % | 1.20 (1.21, 1.20) | 0.00 (0.00, 0.00) |
| user to server floor - RTT/2 ms | 0.11 (0.10, 0.11) | 0.11 (0.11, 0.11) |
| server to user p50 +ms | 19778.2 (19710.9, 19845.5) | 4.9 (4.9, 4.9) |
| server to user p99 +ms | 33854.7 (33670.4, 34039.0) | 15.2 (16.8, 13.6) |
| server to user p99.9 +ms | 34110.8 (33913.4, 34308.1) | 19.6 (22.1, 17.1) |
| server to user jitter p99 ms | 34.2 (34.1, 34.2) | 13.1 (15.0, 11.1) |
| server to user late+lost % | 100.00 (100.00, 100.00) | 0.00 (0.00, 0.00) |
| server to user floor - RTT/2 ms | 2054.38 (2137.38, 1971.38) | 0.38 (0.47, 0.30) |

### `N=1 RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=50 MIX=stream_vs_dl SBR=1.2`

| | `old` (2) | `b2nx` (2) |
|---|---|---|
| **context** | | |
| all users dn / cap_P10 % | 12 (14, 11) | 17 (14, 21) |
| all users up / cap_P10 % | 0 (0, 0) | 0 (0, 0) |
| heavy user mean Mbit/s | 0.44 (0.48, 0.39) | 0.38 (0.32, 0.43) |
| heavy user min Mbit/s | 0.18 (0.16, 0.20) | 0.34 (0.31, 0.36) |
| **stream** | | |
| bitrate / fair share Mbit/s | 1.20 (1.20, 1.20) / 1.86 (1.86, 1.86) | 1.20 (1.20, 1.20) / 1.86 (1.86, 1.86) |
| startup p50 s | inf (inf, inf) | 9.88 (11.88, 7.89) |
| startup max s | inf (inf, inf) | 12.00 (12.42, 11.57) |
| not started | 25 (25, 25) | 0 (0, 0) |
| rebuffers (all viewers) | 0 (0, 0) | 58 (50, 65) |
| viewers with a rebuffer | 0 (0, 0) | 25 (25, 25) |
| rebuffer time s (all) | 0.0 (0.0, 0.0) | 412.1 (407.2, 416.9) |
| segment rate Mbit/s | 0.00 (0.00, 0.00) | 0.28 (0.20, 0.36) |

### `N=1 RATE=100 RTT=72 LOSS=0.5 MODE=fqcodel USERS=50 MIX=gamer_vs_dl`

| | `old` (2) | `b2nx` (2) |
|---|---|---|
| **context** | | |
| all users dn / cap_P10 % | 1 (1, 1) | 1 (1, 1) |
| all users up / cap_P10 % | 1 (1, 1) | 1 (1, 1) |
| heavy user mean Mbit/s | 0.03 (0.03, 0.03) | 0.00 (0.00, 0.00) |
| heavy user min Mbit/s | 0.01 (0.01, 0.02) | 0.00 (0.00, 0.00) |
| **game TCP** | | |
| user to server p50 +ms | 181.2 (172.9, 189.5) | 132.2 (130.3, 134.2) |
| user to server p99 +ms | 5396.5 (3795.5, 6997.4) | 4745.1 (4732.9, 4757.3) |
| user to server p99.9 +ms | 7808.2 (6514.1, 9102.3) | 6484.8 (6962.5, 6007.1) |
| user to server jitter p99 ms | 150.5 (150.4, 150.5) | 104.7 (104.1, 105.2) |
| user to server late+lost % | 85.92 (85.97, 85.87) | 75.28 (74.64, 75.92) |
| user to server floor - RTT/2 ms | 132.35 (135.83, 128.87) | 80.35 (78.07, 82.63) |
| server to user p50 +ms | inf (inf, inf) | 103.4 (102.7, 104.1) |
| server to user p99 +ms | inf (inf, inf) | 8317.2 (16379.6, 254.8) |
| server to user p99.9 +ms | inf (inf, inf) | 8460.4 (16580.1, 340.6) |
| server to user jitter p99 ms | - | 143.2 (175.4, 111.0) |
| server to user late+lost % | 100.00 (100.00, 100.00) | 96.97 (97.48, 96.47) |
| server to user floor - RTT/2 ms | 218.38 (81.01, 355.75) | 96.11 (95.40, 96.81) |
| **game UDP** | | |
| user to server p50 +ms | 18994.1 (19546.1, 18442.0) | 9561.7 (6715.0, 12408.4) |
| user to server p99 +ms | 23634.9 (24149.2, 23120.6) | 11606.5 (9302.8, 13910.2) |
| user to server p99.9 +ms | 23974.1 (24353.3, 23594.8) | 11784.3 (9524.8, 14043.9) |
| user to server jitter p99 ms | 182.3 (208.3, 156.2) | 138.9 (149.4, 128.4) |
| user to server late+lost % | 100.00 (100.00, 100.00) | 100.00 (100.00, 100.00) |
| user to server floor - RTT/2 ms | 1877.90 (2122.50, 1633.30) | 5222.93 (7975.04, 2470.83) |
| server to user p50 +ms | inf (inf, inf) | 8468.7 (8268.9, 8668.4) |
| server to user p99 +ms | inf (inf, inf) | 14380.9 (14060.5, 14701.2) |
| server to user p99.9 +ms | inf (inf, inf) | 14662.0 (14406.2, 14917.7) |
| server to user jitter p99 ms | - | 110.9 (110.5, 111.2) |
| server to user late+lost % | 100.00 (100.00, 100.00) | 99.90 (99.93, 99.87) |
| server to user floor - RTT/2 ms | 242.82 (265.43, 220.21) | 3148.57 (3013.12, 3284.03) |

