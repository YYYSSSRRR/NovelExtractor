# 全量爬取那一轮的现场记录

> `crawl.log` 本身**没有入库**（184KB，每次运行都不同，已在 `.gitignore` 里）。
> 这个文件是把它里面能被命令复算的部分摘出来。要完全复核 15584 这个数，
> 得重跑一轮全量爬取——站点内容会变，重跑的数字不会与这里相同。

## 现场

```
$ head -1 crawl.log
seed=1067 resume-queue=0 workers=32 delay=1.5s budget/host=30 target/host=15

$ tail -1 crawl.log
[23:01:03] pages=15584 articles=9160 hosts=1067 queue=1 inflight=0 3.2 pages/s 失败=613 跳过=0

$ grep -c '' crawl.log        # 日志总行数
1879
```

首行 banner 与 README 6.5 里写的启动参数逐字一致。
**这个日志里没有收尾块**——`grep -c "用时" crawl.log` 是 0，
说明进程从未走到收尾代码，末行停在 `queue=1 inflight=0` 之后二十多分钟不动。

## 计数

```
$ grep -o 'pages=[0-9]*' crawl.log | tail -1
pages=15584

$ grep -c '抓取失败' crawl.log
613

$ grep -c 'robots 禁止' crawl.log
39

$ grep -c '放弃该域' crawl.log    # 修复前没有这条路径
0

$ grep -c 'Crawl-delay' crawl.log  # 修复前也没有上限
0
```

## 失败原因分布

```
$ grep -o '抓取失败.*: \(http [0-9]*\|.*\)$' crawl.log | sed 's/.*: //' | sort | uniq -c | sort -rn
 213 http 404
  75 http 403
  54 too many redirects
  42 context deadline exceeded (Client.Timeout exceeded while awaiting headers)
  32 EOF
  31 connection reset by peer
  28 http 202
  25 http 412
  19 http 406
  18 http 250
  14 http 521
  12 http 405
  12 http 302
   7 no such host
   6 http 503
   6 http 429
   3 http 518
   3 http 502
   3 http 256
   2 connection refused
   1 unexpected EOF
   1 http 550
   1 http 504
   1 http 501
   1 http 500
   1 http 430
   1 http 400
   1 context deadline exceeded (Client.Timeout or context cancellation while reading body)
```

## 时间跨度

```
$ grep -o '^\[[0-9:]*\]' crawl.log | head -1; grep -o '^\[[0-9:]*\]' crawl.log | tail -1
[21:38:53]  →  [23:01:03]
```

`21:38:53` → `23:01:03` 是 **82 分 10 秒 = 4930 秒**，
15584 ÷ 4930 ≈ **3.2 页/秒**（就是末行显示的那个数）。
日志里出现过的最大瞬时速率是 47.1 pages/s。两个口径的差别见 README 6.1。
