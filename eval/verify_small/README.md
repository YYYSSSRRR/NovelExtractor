# 小规模真实爬取：验证修复后的链路

这一轮的目的**不是页数**，而是证明「修复后的爬虫能自己干净结束」——
上一轮全量爬取（15584 页）跑到最后停在 `queue=1 inflight=0` 连续 22 分钟不动，
直到进程被杀，`crawl.log` 里连收尾汇总都没打印出来。本节是那次事故的对照现场。

## 复现命令

```bash
# 种子表就是这条命令产物，20 个域，从全量 1067 个种子里等距取样
awk 'NR%53==1' seeds/seeds.txt | head -20 > eval/verify_small/seeds.txt

go build -o bin/crawler ./cmd/crawler
./bin/crawler -data /tmp/verify -seeds eval/verify_small/seeds.txt \
    -workers 16 -delay 1s -timeout 10s -budget 6 -per-host 2 \
    -max-pages 200 -min-content 300 -trace /tmp/verify.trace
```

`-max-pages 200` 是安全阀，本轮没碰到（实际 59 页）：`-budget 6` 与
`-per-host 2` 已经把这个规模钉死了。

## 结果

见 `run.log`（完整 stderr）、`pages.jsonl`（59 行产物）、`requests.trace`（90 条请求时刻）。

```
用时        11s（4.9 pages/s）
产出页数    59
覆盖域名    19（已收尾 19）
其中正文页  11
抓取失败    2
robots 拒绝 1
Crawl-delay 过长放弃 0
连续失败放弃的域 1
已够 2 篇而收手的域 5（省下 15 次请求）
```

## 四条断言

**1. 进程自己退出，且打印了收尾汇总。** 退出码 0，`run.log` 末尾有
`=== 抓取结束 ===`。上一轮的日志里 `grep -c "用时" crawl.log` = 0——收尾块
一次都没打印过，因为进程根本没走到那里。进度行最后停在
`queue=1 inflight=1`，随后自然排空；没有再出现「计数不变、时间在走」的停滞。

**2. 产物零个不可解析行。**

```bash
python3 - <<'PY'
import json
raw = open("eval/verify_small/pages.jsonl","rb").read()
lines = raw.split(b"\n")
if lines and lines[-1] == b"":
    lines.pop()                      # 正常结尾的换行，不是断行
bad = 0; rows = []
for i, l in enumerate(lines, 1):
    try:
        rows.append(json.loads(l))
    except Exception as e:
        bad += 1
        print("坏行", i, e)
print("行数", len(lines), "可解析", len(rows), "坏行", bad,
      "域名", len({r["host"] for r in rows}))
PY
# 行数 59 可解析 59 坏行 0 域名 19
```

**3. 新增的收手路径真的被走到过。** 这是本节最该看的一栏——修复前这些分支
一条都不会触发：

| 路径 | 本轮 | 证据 |
|---|---|---|
| 已够 N 篇而收手 | **5 个域**（省下 15 次请求） | `已够 2 篇而收手的域 5（省下 15 次请求）` |
| 连续失败放弃 | **1 个域** | `连续失败 2 次，放弃该域剩余 3 条: www.jsbchina.cn` |
| Crawl-delay 过长放弃 | 0 个域 | 这 20 个域里没有声明超过 60s 的站，所以没触发 |

第三类没触发是取样问题，不是代码问题：它的单元测试在
`cmd/crawler/robots_delay_test.go`，端到端那条在
`cmd/crawler/shutdown_test.go` 的 `TestShutdownEscapesAFarFutureTimeslot`
（声明 `Crawl-delay: 30` 的站，取消之后 worker 必须在 5 秒内退出来）。
要在真实站点上撞见它，得先有一个声明小时级间隔的站出现在种子表里。

**4. 同域相邻两次请求的间隔 ≥ `-delay`。** 这一条无法从结果反推，只能量：

```bash
python3 - <<'PY'
import datetime
prev={}; viol=[]; gaps=[]
for line in open("eval/verify_small/requests.trace",encoding="utf-8"):
    ts,host,url=line.rstrip("\n").split("\t")
    t=datetime.datetime.fromisoformat(ts)
    if host in prev:
        g=(t-prev[host]).total_seconds(); gaps.append(g)
        if g < 1.0 - 1e-6: viol.append((host,g))
    prev[host]=t
print("样本",len(gaps),"最小 %.3fs"%min(gaps),"违反",len(viol))
PY
# 样本 70 最小 1.000s 违反 0
```

最小间隔正好是 1.000s，一次都没有提前。上一轮用同样的方法量到过 32 对
低于配置值的间隔，全部是 robots.txt 与紧随其后的页面请求——那正是 `Gate`
要修的事，`requests.trace` 里 20 个域各含一次 robots 请求，都在这 70 个样本里。

## 这一轮**没有**证明的事

- 没有证明大规模下的吞吐。11 秒 59 页是 20 个域、每域预算 6 的小规模数字，
  与全量 1067 个域不可比（见 README 6.1 对吞吐口径的说明）。
- 没有证明抽取质量。59 行里 35 行抽取为空，是因为 `-min-content 300` 加上
  样本里大量首页/列表页，这属于**页面选取**而非算法表现；抽取质量的实测见
  README 第七节与第 6.4 节的抽检。
- 没有覆盖 `Crawl-delay 过长` 这条路径，理由见上。
