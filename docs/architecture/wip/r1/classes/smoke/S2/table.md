### `N=8 RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=64 MIX=home SBR=1.2`

| | `old` (2) | `b2nx` (2) |
|---|---|---|
| **context** | | |
| all users dn / cap_P10 % | 97 (97, 97) | 93 (94, 93) |
| all users up / cap_P10 % | 91 (92, 90) | 90 (90, 91) |
| heavy user mean Mbit/s | 6.11 (6.23, 5.99) | 4.45 (3.95, 4.95) |
| heavy user min Mbit/s | 5.00 (5.03, 4.96) | 1.71 (1.50, 1.92) |
| **game TCP** | | |
| user to server p50 +ms | 67.4 (134.3, 0.5) | 0.4 (0.3, 0.6) |
| user to server p99 +ms | 679.2 (836.3, 522.1) | 2.0 (1.8, 2.2) |
| user to server p99.9 +ms | 911.2 (1084.9, 737.5) | 5.5 (5.5, 5.5) |
| user to server jitter p99 ms | 123.7 (166.3, 81.1) | 2.4 (2.2, 2.5) |
| user to server late+lost % | 36.05 (53.93, 18.18) | 0.00 (0.00, 0.00) |
| user to server floor - RTT/2 ms | 0.12 (0.13, 0.12) | 0.11 (0.11, 0.12) |
| server to user p50 +ms | 141.2 (191.4, 90.9) | 6.5 (7.0, 6.0) |
| server to user p99 +ms | 682.7 (845.1, 520.3) | 74.0 (70.4, 77.7) |
| server to user p99.9 +ms | 863.4 (1106.1, 620.7) | 111.9 (105.9, 117.9) |
| server to user jitter p99 ms | 194.3 (215.0, 173.6) | 33.8 (33.7, 34.0) |
| server to user late+lost % | 57.10 (69.14, 45.06) | 0.29 (0.22, 0.36) |
| server to user floor - RTT/2 ms | 4.77 (4.10, 5.43) | 3.07 (2.61, 3.54) |
| **game UDP** | | |
| user to server p50 +ms | 115.4 (192.7, 38.1) | 0.3 (0.2, 0.3) |
| user to server p99 +ms | 550.6 (573.7, 527.4) | 1.8 (1.5, 2.1) |
| user to server p99.9 +ms | 713.6 (695.5, 731.6) | 5.6 (5.2, 5.9) |
| user to server jitter p99 ms | 169.8 (181.5, 158.2) | 2.5 (2.1, 2.9) |
| user to server late+lost % | 55.99 (66.99, 44.99) | 0.00 (0.00, 0.00) |
| user to server floor - RTT/2 ms | 0.15 (0.17, 0.13) | 0.12 (0.12, 0.13) |
| server to user p50 +ms | 203.5 (215.0, 192.0) | 6.3 (6.6, 5.9) |
| server to user p99 +ms | 698.9 (724.4, 673.3) | 76.2 (73.8, 78.6) |
| server to user p99.9 +ms | 824.1 (877.9, 770.2) | 109.6 (109.3, 109.9) |
| server to user jitter p99 ms | 196.2 (198.4, 194.0) | 41.7 (34.0, 49.4) |
| server to user late+lost % | 74.81 (79.39, 70.22) | 0.28 (0.25, 0.31) |
| server to user floor - RTT/2 ms | 5.60 (5.32, 5.88) | 3.28 (3.11, 3.45) |
| **call** | | |
| user to server p50 +ms | 24.5 (40.6, 8.3) | 9.6 (4.0, 15.2) |
| user to server p99 +ms | 903.2 (808.4, 998.0) | 422.8 (373.4, 472.3) |
| user to server p99.9 +ms | 1141.6 (1057.0, 1226.1) | 585.5 (569.4, 601.6) |
| user to server jitter p99 ms | 166.4 (168.0, 164.9) | 87.6 (81.0, 94.2) |
| user to server late+lost % | 36.85 (40.00, 33.69) | 10.30 (5.70, 14.89) |
| user to server floor - RTT/2 ms | 1.04 (1.24, 0.84) | 0.78 (0.70, 0.86) |
| user to server delivered % | 100.0 (100.0, 100.0) | 100.0 (100.0, 100.0) |
| server to user p50 +ms | 8845.6 (9656.2, 8035.1) | 259.9 (25.6, 494.2) |
| server to user p99 +ms | 19665.3 (18009.1, 21321.5) | 6979.9 (1058.4, 12901.5) |
| server to user p99.9 +ms | 20226.3 (18379.3, 22073.3) | 7569.4 (1785.4, 13353.4) |
| server to user jitter p99 ms | 224.0 (243.6, 204.4) | 78.5 (71.6, 85.5) |
| server to user late+lost % | 99.95 (99.96, 99.95) | 39.32 (14.32, 64.31) |
| server to user floor - RTT/2 ms | 2065.01 (2422.37, 1707.64) | 5.15 (1.18, 9.12) |
| server to user delivered % | 86.6 (84.5, 88.6) | 98.0 (100.0, 96.0) |
| **stream** | | |
| bitrate / fair share Mbit/s | 1.20 (1.20, 1.20) / 2.64 (2.68, 2.61) | 1.20 (1.20, 1.20) / 2.48 (2.38, 2.59) |
| startup p50 s | 3.23 (3.45, 3.01) | 4.26 (3.14, 5.38) |
| startup max s | 3.75 (3.87, 3.63) | 6.67 (5.63, 7.70) |
| not started | 0 (0, 0) | 0 (0, 0) |
| rebuffers (all viewers) | 50 (52, 47) | 16 (17, 14) |
| viewers with a rebuffer | 16 (16, 16) | 8 (7, 8) |
| rebuffer time s (all) | 158.8 (168.4, 149.2) | 62.9 (73.4, 52.5) |
| segment rate Mbit/s | 0.78 (0.76, 0.79) | 1.16 (1.28, 1.04) |
| **web** | | |
| page load p50 ms | 6619 (6679, 6559) | 5811 (5870, 5753) |
| page load p95 ms | 8232 (8086, 8377) | 10240 (8754, 11726) |
| first byte p50 ms | 378 (463, 293) | 148 (139, 156) |
| first byte p95 ms | 746 (877, 616) | 215 (219, 211) |
| pages (failed) | 50 (49, 50) (0 (0, 0)) | 44 (50, 38) (0 (1, 0)) |

### `N=8 RATE=100 RTT=72 LOSS=0 MODE=pfifo:600 USERS=64 MIX=home SBR=1.2`

| | `old` (2) | `b2nx` (2) |
|---|---|---|
| **context** | | |
| all users dn / cap_P10 % | 96 (96, 96) | 91 (85, 97) |
| all users up / cap_P10 % | 91 (91, 92) | 85 (85, 85) |
| heavy user mean Mbit/s | 5.90 (5.99, 5.80) | 6.18 (5.28, 7.08) |
| heavy user min Mbit/s | 4.79 (4.87, 4.71) | 4.01 (3.81, 4.21) |
| **game TCP** | | |
| user to server p50 +ms | 49.3 (42.7, 55.9) | 0.3 (0.2, 0.4) |
| user to server p99 +ms | 502.8 (259.7, 745.9) | 35.4 (25.8, 45.0) |
| user to server p99.9 +ms | 718.4 (537.7, 899.1) | 42.4 (36.0, 48.8) |
| user to server jitter p99 ms | 111.2 (41.1, 181.2) | 2.5 (2.4, 2.6) |
| user to server late+lost % | 21.40 (9.15, 33.65) | 0.00 (0.00, 0.00) |
| user to server floor - RTT/2 ms | 0.20 (0.24, 0.15) | 0.10 (0.10, 0.10) |
| server to user p50 +ms | 110.6 (161.8, 59.5) | 42.0 (42.2, 41.9) |
| server to user p99 +ms | 529.5 (549.8, 509.3) | 146.9 (137.2, 156.6) |
| server to user p99.9 +ms | 719.7 (787.2, 652.1) | 192.6 (190.9, 194.3) |
| server to user jitter p99 ms | 189.8 (185.1, 194.5) | 33.3 (33.3, 33.3) |
| server to user late+lost % | 52.11 (64.17, 40.06) | 1.83 (1.44, 2.22) |
| server to user floor - RTT/2 ms | 5.27 (4.78, 5.76) | 0.11 (0.11, 0.11) |
| **game UDP** | | |
| user to server p50 +ms | 47.7 (40.6, 54.8) | 0.3 (0.3, 0.3) |
| user to server p99 +ms | 452.3 (266.8, 637.9) | 35.5 (26.0, 45.0) |
| user to server p99.9 +ms | 726.5 (600.9, 852.0) | 42.4 (36.3, 48.5) |
| user to server jitter p99 ms | 108.9 (37.9, 180.0) | 2.5 (2.4, 2.6) |
| user to server late+lost % | 21.35 (8.89, 33.82) | 0.00 (0.00, 0.00) |
| user to server floor - RTT/2 ms | 0.18 (0.20, 0.16) | 0.10 (0.10, 0.09) |
| server to user p50 +ms | 108.5 (158.1, 58.9) | 41.9 (42.2, 41.5) |
| server to user p99 +ms | 494.2 (473.6, 514.9) | 153.3 (141.8, 164.9) |
| server to user p99.9 +ms | 759.8 (805.2, 714.3) | 195.4 (193.8, 197.0) |
| server to user jitter p99 ms | 186.7 (180.9, 192.5) | 33.1 (32.9, 33.3) |
| server to user late+lost % | 52.11 (63.06, 41.17) | 1.79 (1.47, 2.11) |
| server to user floor - RTT/2 ms | 5.31 (5.16, 5.46) | 0.13 (0.12, 0.14) |
| **call** | | |
| user to server p50 +ms | 51.7 (46.4, 57.0) | 17.6 (20.1, 15.1) |
| user to server p99 +ms | 455.8 (268.1, 643.4) | 227.8 (247.9, 207.8) |
| user to server p99.9 +ms | 622.9 (377.0, 868.8) | 320.3 (328.9, 311.7) |
| user to server jitter p99 ms | 124.9 (62.3, 187.5) | 19.8 (20.1, 19.5) |
| user to server late+lost % | 19.49 (8.37, 30.61) | 1.87 (2.17, 1.56) |
| user to server floor - RTT/2 ms | 1.11 (1.41, 0.82) | 0.98 (0.89, 1.07) |
| user to server delivered % | 100.0 (100.0, 100.0) | 100.0 (100.0, 100.0) |
| server to user p50 +ms | 9667.1 (10566.4, 8767.8) | 11355.2 (15682.4, 7028.0) |
| server to user p99 +ms | 19168.5 (17897.5, 20439.4) | 27201.8 (27973.2, 26430.4) |
| server to user p99.9 +ms | 19599.8 (18421.1, 20778.5) | 27631.2 (28488.7, 26773.8) |
| server to user jitter p99 ms | 239.1 (247.0, 231.1) | 220.9 (204.4, 237.3) |
| server to user late+lost % | 100.00 (100.00, 100.00) | 98.49 (98.08, 98.89) |
| server to user floor - RTT/2 ms | 2335.24 (2249.38, 2421.09) | 47.99 (39.70, 56.28) |
| server to user delivered % | 92.2 (87.8, 96.6) | 48.6 (51.7, 45.6) |
| **stream** | | |
| bitrate / fair share Mbit/s | 1.20 (1.20, 1.20) / 2.62 (2.65, 2.59) | 1.20 (1.20, 1.20) / 2.66 (2.61, 2.71) |
| startup p50 s | 2.81 (2.70, 2.92) | 2.95 (2.68, 3.21) |
| startup max s | 3.92 (3.74, 4.09) | 4.88 (4.71, 5.06) |
| not started | 0 (0, 0) | 0 (0, 0) |
| rebuffers (all viewers) | 52 (54, 49) | 45 (46, 44) |
| viewers with a rebuffer | 16 (16, 16) | 16 (16, 16) |
| rebuffer time s (all) | 160.0 (157.7, 162.3) | 328.1 (322.5, 333.8) |
| segment rate Mbit/s | 0.78 (0.78, 0.77) | 0.44 (0.41, 0.47) |
| **web** | | |
| page load p50 ms | 6022 (6112, 5932) | 6722 (6107, 7336) |
| page load p95 ms | 8440 (7401, 9479) | inf (11053, inf) |
| first byte p50 ms | 309 (295, 323) | 170 (154, 186) |
| first byte p95 ms | 787 (735, 839) | 241 (235, 247) |
| pages (failed) | 51 (51, 51) (0 (0, 0)) | 54 (57, 50) (2 (0, 3)) |

### `N=1 RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=50 MIX=gamer_vs_dl CONNS=4`

| | `old` (1) | `b2nx` (1) |
|---|---|---|
| **context** | | |
| all users dn / cap_P10 % | 95 | 92 |
| all users up / cap_P10 % | 1 | 1 |
| heavy user mean Mbit/s | 3.45 | 3.36 |
| heavy user min Mbit/s | 3.13 | 2.83 |
| **game TCP** | | |
| user to server p50 +ms | 0.9 | 1.0 |
| user to server p99 +ms | 300.4 | 3.9 |
| user to server p99.9 +ms | 383.6 | 4.8 |
| user to server jitter p99 ms | 16.2 | 3.1 |
| user to server late+lost % | 3.13 | 0.00 |
| user to server floor - RTT/2 ms | 0.15 | 0.11 |
| server to user p50 +ms | 549.6 | 0.2 |
| server to user p99 +ms | 1018.6 | 0.6 |
| server to user p99.9 +ms | 1120.0 | 1.0 |
| server to user jitter p99 ms | 300.1 | 0.5 |
| server to user late+lost % | 97.65 | 0.00 |
| server to user floor - RTT/2 ms | 5.86 | 0.12 |
| **game UDP** | | |
| user to server p50 +ms | 0.8 | 0.7 |
| user to server p99 +ms | 306.4 | 3.8 |
| user to server p99.9 +ms | 377.6 | 4.9 |
| user to server jitter p99 ms | 15.6 | 3.0 |
| user to server late+lost % | 3.29 | 0.00 |
| user to server floor - RTT/2 ms | 0.13 | 0.10 |
| server to user p50 +ms | 549.1 | 0.2 |
| server to user p99 +ms | 1017.4 | 0.6 |
| server to user p99.9 +ms | 1123.4 | 1.0 |
| server to user jitter p99 ms | 300.4 | 0.5 |
| server to user late+lost % | 97.69 | 0.00 |
| server to user floor - RTT/2 ms | 6.01 | 0.12 |

### `N=1 RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=50 MIX=stream_vs_dl CONNS=4 SBR=1.2`

| | `old` (1) | `b2nx` (1) |
|---|---|---|
| **context** | | |
| all users dn / cap_P10 % | 97 | 96 |
| all users up / cap_P10 % | 0 | 0 |
| heavy user mean Mbit/s | 2.13 | 1.73 |
| heavy user min Mbit/s | 1.99 | 1.49 |
| **stream** | | |
| bitrate / fair share Mbit/s | 1.20 / 1.86 | 1.20 / 1.86 |
| startup p50 s | 1.67 | 0.24 |
| startup max s | 2.27 | 0.31 |
| not started | 0 | 0 |
| rebuffers (all viewers) | 0 | 0 |
| viewers with a rebuffer | 0 | 0 |
| rebuffer time s (all) | 0.0 | 0.0 |
| segment rate Mbit/s | 1.58 | 1.97 |

### `N=8 RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=50 MIX=gamer_vs_dl`

| | `old` (1) | `b2nx` (1) |
|---|---|---|
| **context** | | |
| all users dn / cap_P10 % | 85 | 26 |
| all users up / cap_P10 % | 1 | 1 |
| heavy user mean Mbit/s | 3.09 | 0.90 |
| heavy user min Mbit/s | 1.75 | 0.62 |
| **game TCP** | | |
| user to server p50 +ms | 0.2 | 0.3 |
| user to server p99 +ms | 59.4 | 2.1 |
| user to server p99.9 +ms | 176.6 | 3.1 |
| user to server jitter p99 ms | 14.3 | 1.9 |
| user to server late+lost % | 0.71 | 0.00 |
| user to server floor - RTT/2 ms | 0.10 | 0.11 |
| server to user p50 +ms | 86.6 | 4.7 |
| server to user p99 +ms | 295.0 | 14.1 |
| server to user p99.9 +ms | 388.7 | 17.8 |
| server to user jitter p99 ms | 62.6 | 12.1 |
| server to user late+lost % | 34.97 | 0.00 |
| server to user floor - RTT/2 ms | 40.54 | 0.35 |
| **game UDP** | | |
| user to server p50 +ms | 0.2 | 0.3 |
| user to server p99 +ms | 46.3 | 2.1 |
| user to server p99.9 +ms | 170.3 | 3.2 |
| user to server jitter p99 ms | 13.2 | 1.8 |
| user to server late+lost % | 0.65 | 0.00 |
| user to server floor - RTT/2 ms | 0.09 | 0.11 |
| server to user p50 +ms | 13905.5 | 4.8 |
| server to user p99 +ms | 23033.4 | 14.6 |
| server to user p99.9 +ms | 24502.4 | 18.1 |
| server to user jitter p99 ms | 60.8 | 12.6 |
| server to user late+lost % | 100.00 | 0.00 |
| server to user floor - RTT/2 ms | 615.53 | 0.32 |

### `N=8 RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=50 MIX=stream_vs_dl SBR=1.2`

| | `old` (1) | `b2nx` (1) |
|---|---|---|
| **context** | | |
| all users dn / cap_P10 % | 75 | 41 |
| all users up / cap_P10 % | 0 | 0 |
| heavy user mean Mbit/s | 2.63 | 0.80 |
| heavy user min Mbit/s | 1.32 | 0.52 |
| **stream** | | |
| bitrate / fair share Mbit/s | 1.20 / 1.86 | 1.20 / 1.86 |
| startup p50 s | 14.66 | 14.57 |
| startup max s | 15.01 | 15.56 |
| not started | 0 | 0 |
| rebuffers (all viewers) | 34 | 21 |
| viewers with a rebuffer | 25 | 18 |
| rebuffer time s (all) | 370.7 | 269.7 |
| segment rate Mbit/s | 0.17 | 1.53 |

