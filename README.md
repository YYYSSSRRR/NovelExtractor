# web-extract

> 站点无关的通用网页正文抽取器 + 礼貌并发爬虫。

web-extract 从任意网站的 HTML 里抽出**用户在页面上真正看到的标题与正文**。它不依赖站点名、
不依赖 `class="content"` 这类关键词表、不需要任何标注数据，因此可以直接扔给一千个从没见过的
域名。仓库同时提供一个配套爬虫：给一份种子域名表，它会按域礼貌地把页面抓下来、就地抽取、
在域的边界上做跨页收尾并落盘。

两个部分可以独立使用，也可以组合成「种子表 → 抓取 → 抽取 → JSONL 语料」的完整链路。

## 特性

- **不读 class/id。** 全程不出现任何站点专属选择器，判据只有三类：HTML 标准语义标签、
  文本统计量、DOM 结构。唯一一处例外是测试夹具里故意放的 `class="novelinfo"`——它存在的
  意义正是验证算法不看它。
- **跨页统计。** 站点名后缀、模板片段这些「域名级常量」在单页里无从判断，爬虫手里有每域
  十几到上百页，于是可以学出来而不是配出来。页数越多越准。
- **一个结构拿到四个性质。** per-host 队列 + 就绪最小堆，同时实现每域严格串行、全局并行、
  礼貌限速与背压，限速只有一个真相源。
- **无标注评测。** 四条不需要人工标注的代理指标（空结果率、标点密度、跨页模板残留率、
  单行输出占比），对所有实现是同一把尺子。
- **依赖极少。** 除标准库外只用到 `golang.org/x/net`（HTML 解析）与 `golang.org/x/text`。
- **可复算。** 交付语料与跑批产物入库并附 md5，关键实验给出了可重跑的命令。

## 环境要求

- Go 1.26 或更高（`go.mod` 声明 `go 1.26.0`）
- Python 3（仅 `scripts/sample.py` 需要，用于语料抽样）
- 爬虫需要外网访问

## 快速开始

### 构建

```bash
go build -o bin/crawler   ./cmd/crawler
go build -o bin/extractor ./cmd/extractor
go build -o bin/seeds     ./cmd/seeds
go build -o bin/eval      ./cmd/eval
```

### 一个最小示例

抽取器的接口是 stdin 逐行 JSON → stdout 逐行 JSON。造一行输入：

```bash
cat > /tmp/demo.jsonl <<'EOF'
{"url": "https://example.com/news/1", "title": "示例新闻 - 示例网", "html": "<html><head><title>示例新闻 - 示例网</title></head><body><nav><a href='/'>首页</a><a href='/t'>科技</a></nav><div class='wrapper'><h1>示例新闻</h1><p>第一段正文，用来演示抽取器如何在没有任何 class/id 先验的情况下找到正文容器。它依靠的是文本密度、标点密度和链接密度这三个统计量。</p><p>第二段正文。正文从来不是一个块，而是一串并列的段落，所以打分之后还要把分数沿祖先链累积两层，才能把整篇文章收进同一个容器。</p></div><footer>版权所有 © 示例网</footer></body></html>"}
EOF

./bin/extractor < /tmp/demo.jsonl
```

```json
{"url":"https://example.com/news/1","meta_title":"示例新闻 - 示例网","real_title":"示例新闻","main_content":"第一段正文，用来演示抽取器如何在没有任何 class/id 先验的情况下找到正文容器。它依靠的是文本密度、标点密度和链接密度这三个统计量。\n第二段正文。正文从来不是一个块，而是一串并列的段落，所以打分之后还要把分数沿祖先链累积两层，才能把整篇文章收进同一个容器。"}
```

导航栏与页脚的文案都被丢掉了，`real_title` 从 `示例新闻 - 示例网` 里剥掉了站点后缀，
两个 `<p>` 合并成一段带换行的正文——而输入里没有任何一处告诉它「哪里是正文」。

### 爬虫

```bash
# -data 一定要指向空目录或专门的数据目录：store 是 O_APPEND 追加写，
# 启动时还会做一次破坏性截尾行。用仓库自带的 data/ 会把上万行追加到交付语料后面。
./bin/crawler -data /tmp/fullrun -seeds seeds/seeds.txt \
          -workers 32 -delay 1500ms -budget 30 -per-host 15 \
          -trace /tmp/fullrun/requests.trace
```

中断后重跑同一条命令即续爬。产物落在 `-data` 指定的目录里：

| 文件 | 内容 |
|---|---|
| `pages.jsonl` | 一行一页：抽取结果 + 抓取元数据，追加写 |
| `queue.jsonl` | 待抓队列，与已抓集合合起来才是完整的续爬状态 |
| `crawler.lock` | 数据目录的排他锁，防止两个进程写同一份产物 |

### 种子表与语料抽样

```bash
# 三类来源并联：基础题数据集里的域名（零成本）、人工体裁兜底清单、目录站分类页扩散。
# data/novel.json 不入库，没有它就去掉 -jsonl 那一项。
./bin/seeds -jsonl data/novel.json -extra seeds/manual.txt \
        -harvest seeds/directories.txt -sections 25 \
        -probe -out seeds/seeds.txt

# 抽样规则见 scripts/sample.py：按域分组等距取样，不含随机数，同一份输入两次跑逐字节相同
python3 scripts/sample.py --per-host 2 --cap 300 < /tmp/fullrun/pages.jsonl > data/pages.jsonl
```

## 使用说明

### extractor — 逐行抽取

```bash
./bin/extractor < input.jsonl > output.jsonl      # {url,title,html} → {url,meta_title,real_title,main_content}
./bin/extractor -stats < input.jsonl > /dev/null  # 汇总指标打到 stderr
```

- **输入几行就输出几行。** 坏 JSON 行也占一格（四个字段全空），不静默丢行——下游按行号对齐
  才不会错位。
- stdout 只有 JSON，`-stats` 只往 stderr 写，所以重定向到文件是干净的。
- 同一份输入两次跑逐字节相同。

| 参数 | 默认 | 含义 |
|---|---|---|
| `-batch` | 256 | 批大小：批内并行处理，决定内存上界 |
| `-stats` | false | 处理结束后向 stderr 打印汇总指标 |
| `-no-punct` | false | 消融：关闭标点率因子 |
| `-no-link` | false | 消融：关闭链接密度惩罚 |
| `-no-propagate` | false | 消融：关闭祖先累积（退化为选单块最高分） |
| `-no-cross-page` | false | 消融：关闭跨页站点模板学习（退化为纯单页抽取） |
| `-keep-attribution` | false | 保留「本文来自 xxx(http://...)」这类署名行 |

对题目下发的数据集（`data/novel.json`，126MB，不入库）跑一轮：

```bash
./bin/extractor -stats < data/novel.json > data/novel_out.jsonl
```

### crawler — 并发抓取

| 参数 | 默认 | 含义 |
|---|---|---|
| `-data` | `data` | 数据目录，产物与续爬状态落在这里 |
| `-seeds` | 空 | 种子文件，一行一个 URL 或域名 |
| `-workers` | 32 | 并发 worker 数（= 同时在抓的不同域数） |
| `-delay` | 1500ms | 同一域名两次请求的最小间隔 |
| `-timeout` | 20s | 单次请求超时 |
| `-budget` | 12 | 每域抓取总预算（含首页与栏目页） |
| `-per-host` | 5 | 每域目标正文页数，0 为不限 |
| `-min-content` | 300 | 判为正文页的最小正文长度 |
| `-max-link-rate` | 0.3 | 判为正文页的最大正文链接密度 |
| `-max-host-failures` | 2 | 连续这么多次失败就放弃该域剩余队列，0 为关闭 |
| `-max-pages` | 0 | 全局页数上限，0 为不限 |
| `-max-bytes` | 5MB | 响应体上限 |
| `-retries` | 0 | 单个 URL 的失败重试次数，0 表示抓不到就跳过 |
| `-trace` | 空 | 逐行记录每次请求的发起时刻，用于验证每域请求间隔 |
| `-ua` | 内置 | User-Agent（单一、诚实，不轮换） |
| `-no-templates` | false | 消融：关闭跨页模板过滤 |

`Crawl-delay` 超过 60 秒的站会被整个放弃——照做结束不了，不照做又不诚实，第三条路是不去。

### seeds — 种子表构建

| 参数 | 默认 | 含义 |
|---|---|---|
| `-jsonl` | 空 | 从 `{url,...}` JSONL 里提取域名（最廉价的一批） |
| `-extra` | 空 | 人工兜底清单，一行一个域名/URL |
| `-harvest` | 空 | 目录站 URL 清单，站内翻一层、站外只扩散一层 |
| `-sections` | 8 | 每个目录站最多再翻几个栏目页 |
| `-probe` | false | 入表前探测可达性（强烈建议开启） |
| `-min-cjk` | 50 | 首页至少含多少个中日韩字符才算「活的」 |
| `-workers` / `-timeout` | 32 / 6s | 探测阶段的并发与超时 |
| `-out` | 空 | 输出路径，留空则打到 stdout |

### eval — 对比与消融实验台

```bash
./bin/eval -domains 150 -per-host 4 -out eval
```

现场抓一批真实页面，让多个配置跑同一批，输出逐项报告。页面入样只依据 URL 形态与整页可见
文本长度，**不经过任何被测实现的判据**——否则「哪些页面进入样本」本身就由被测实现决定了。

`-baseline` 默认指向一个本仓库里没有的对照二进制，加 `-baseline ""` 可跳过它。

## 工作原理

### 抽取：只看统计形态与 DOM 结构

**铁律：不读 class/id。** 核心洞察是——**正文区和导航区的区别不在名字，而在统计形态**。
正文是高字符密度、高标点密度、极低链接密度；导航恰好相反。

**① 块打分。** 对每个块级元素数四个量——字符数 `T`、元素个数 `E`、标点数 `P`、`<a>` 后代
字符数 `L`——四个因子相乘：

```
score = log1p(T / (E+1)) × (0.2 + 0.8 × min((P/T)/0.15, 1)) × (1 − L/T) × min(T/200, 1)
        └ 字符密度 ┘      └──────── 标点率 ────────┘   └ 链接密度 ┘  └ 长度闸 ┘
```

量纲感受：100 字的 `<p>`，density≈50、punctRate≈0.12、linkRate=0 → score≈2.9；50 条链接的
导航 div，density≈4、linkRate≈0.9 → score≈0.14。**相差 20 倍。** 用乘性组合而不是加权求和
是刻意的：加权求和下只要某一项极高就能掩盖其余项的缺陷，而正文必须四项同时成立。

**② 祖先累积。** 正文从来不是**一个**块，而是一串并列的 `<p>`，单看任何一个都又短又没
信息量。所以把分数沿祖先链向上汇入两层（`own + Σ子块 + 0.5×Σ孙块`）——上限两层是必须的，
无限上溯会让最外层的整页 wrapper 因为「包含所有内容」而必然胜出。

**③ 容器上溯 + 判定。** 父节点累积分 ≥ 自身 × 0.75 且链接密度没有明显恶化时继续上溯，解决
「正文被多包了一层 wrapper」。最后按四条结构性判据决定「这一页到底有没有正文」：容器是
`html`/`body`、容器是非散文元素、链接密度 > 0.5、短到不成句。判掉了就**交白卷**，而不是把
整页样板以「正文」的名义交出去。

**④ 标题。** `<title>` 几乎总被污染成 `文章标题-栏目-站点名`，所以收集 h1 / og:title /
twitter:title / JSON-LD headline / `<title>` 按分隔符切出的全部前缀组合，**全部进池打分**
而不是按可信度取第一个。权重最高的一项是**候选与正文前 200 字的 bigram 重合率**——站点名、
栏目名不会出现在正文段落里，而真实标题几乎总在正文开头被重复一次，这个信号很难被模板伪造。

**⑤ 跨页学习。** 站点名后缀是**域名级常量**，不是页级性质。对某域名的所有 `<title>` 求
最长公共前后缀（逐位多数投票，≥60% 页面共现），再从标题两端剥掉——不需要站点名单、不需要
正则、不知道体裁，而且页数越多越准。模板过滤同理，但判据是**文档频率**而非词频：一个片段
在一页里出现 100 次说明它在重复自己（那可能正是正文的修辞），在 100 页里的 60 页各出现
1 次才说明它是站点模板。

### 爬虫：per-host 队列 + 就绪最小堆

直接开 500 个 goroutine 抢一个 URL channel，会同时向同一个站发几十个并发请求，几分钟内被
ban。**「全局并发」和「每域礼貌」是正交的两个约束，必须分开管。**

```
Frontier
  queues  map[host]*hostQueue   host → FIFO URL 队列
  heap    hostHeap              最小堆，按 nextAt（该域下次可请求的时刻）排序
  ready   chan Task             cap = worker 数
  seen    map[uint64]struct{}   URL 规范化后的 hash，去重

调度循环（单协程）:
  取堆顶；若 nextAt 未到 → 睡到那个时刻（有新域入堆则重新判定）
  弹出，从该域队列取一条 URL，交给 ready；ready 满则阻塞 —— 天然背压

worker:
  for host := range ready { 取该域队列头部 → 抓取 → Done(host)（nextAt = now + delay）→ 重新入堆 }
```

一个结构同时拿到四个性质：

| 性质 | 如何得到 |
|---|---|
| **每域严格串行** | 一个 host 要么在堆里、要么在某个 worker 手里，不可能同时在两处 |
| **全局并行** | ready 由 N 个 worker 消费，不同 host 天然并行 |
| **礼貌限速** | 时间门控下沉进堆，`nextAt` 是唯一真相源，业务代码里没有散落的 sleep |
| **背压** | ready 有界，堆满即阻塞调度器，不会无限堆积 URL |

**每站抓什么**用四级漏斗，便宜的先筛、贵的后验：首页 + ≤2 个栏目页抽同域链接 →
URL 粗筛（纯 CPU、零成本）→ 抓取限额 `-budget` → **用抽取器自己做页面类型分类器**
（正文长度 ≥300 且链接密度 ≤0.3）。最后一步是自举设计：复用同一套代码，不引入第二套站点
知识，比 URL 正则可靠得多。

**什么时候可以不抓了。** 按域礼貌限速意味着 `域数 × 每域请求数 × 间隔 ÷ worker 数` 就是
系统耗时。三条收手路径都只丢**队列**、不丢已抓到的页面：已够数（`-per-host`）、连续失败
（`-max-host-failures`）、站点声明的 `Crawl-delay` 超过上限。

> 算法每一步的推导、踩过的坑、以及每个设计决策的取舍理由，见 [NOTES.md](NOTES.md)。

## 仓库结构

```
cmd/crawler       爬虫主程序：调度、抓取、四级漏斗、域边界收尾
cmd/extractor     基础题接口：stdin 逐行 JSON → stdout 逐行 JSON（输入几行输出几行）
cmd/seeds         种子表构建：三类来源并联 + 可达性探测
cmd/eval          对比与消融实验台：现场抓一批页面，多配置跑同一批

internal/extract  抽取算法：块打分、容器选择、标题、跨页模板
internal/frontier per-host 调度器：队列 + 最小堆 + 时间闸 + 背压
internal/fetch    抓取：编码统一、超时、robots.txt、响应体上限
internal/discover 链接发现与文章页 URL 粗筛
internal/seed     种子来源合并与去重
internal/store    JSONL 落盘、断点续爬、排他锁

data/             交付语料与跑批产物（见下）
seeds/            种子表：seeds.txt 全量种子域、manual.txt 人工兜底、directories.txt 目录站
scripts/sample.py 交付语料的抽样脚本（只写 stdout，不接受任何写文件的用法）
```

5209 行 Go（另有 2628 行测试），依赖 `golang.org/x/net` + `golang.org/x/text`。

## 数据说明

| 文件 | 内容 | 校验 |
|---|---|---|
| `data/novel_out.jsonl` | 基础题数据集全量跑批产物，3473 行 | md5 `e1d56e337a34e873595197cfd83850de` |
| `data/pages.jsonl` | 交付语料，300 行 / 300 个域名 | md5 `577444028f373a452566133e64b1a347` |
| `seeds/seeds.txt` | 全量爬取用的 1067 个种子域 | — |
| `data/novel.json` | 题目下发数据集，126MB，**不入库** | 需自行获取 |

抽取产物的字段：

| 字段 | 含义 |
|---|---|
| `url` | 输入 URL |
| `meta_title` | `<head><title>` 的纯文本 |
| `real_title` | 用户在页面上看到的标题（剥掉站点模板成分） |
| `main_content` | 正文纯文本，块级边界为 `\n`；判定为「没有正文」时为空串 |

爬虫产物 `pages.jsonl` 在此基础上追加 `final_url` / `host` / `status` / `bytes` /
`html_sha1` / `elapsed_ms` / `fetched_at` / `content_chars` / `content_link_rate`。

**语料里混着非文章页**：`data/pages.jsonl` 存的是所有抓到的页面，不是只存文章页。这是有意
的取舍，不是缺陷，读这份样本时要在「这份样本的构成」这个意义上读。详细的构成分析见
[NOTES.md](NOTES.md)。

## 测试

```bash
go test ./...        # 66 个测试函数
go vet ./...         # 无输出
gofmt -l .           # 无输出
```

**测试大多是回归测试，每一条都对应一个真实发生过的 bug**，注释里写了现场。最值得看的四条：

- `cmd/crawler/stall_test.go` —— 一个只会超时的域必须在有限次失败之后被放弃，整轮爬取必须
  能自己结束。对应一次停在 `queued=1 inflight=0` 二十多分钟的事故。
- `cmd/crawler/shutdown_test.go` —— 调度器正睡在某个域远期的时间片上时收到 SIGTERM，worker
  必须能在有限时间内退出来，好让内存里那批页面落盘。
- `internal/frontier/invariant_test.go` —— 随机并发下四条调度不变量。其中「`state=idle` 的域
  必无 URL」正是那次事故的形状：有 URL 躺在一个不在堆里的域中，而三个公开计数怎么读都自洽。
- `cmd/extractor/contract_test.go` —— 与外部系统之间的接口约定：输入几行就输出几行（坏 JSON
  也占一格）、每行恰好那四个字段、stdout 只有 JSON。


## 已知限制

- **语料里混着非文章页。** 判据看的是文本的统计形态，而纯列表页的统计形态并不比一篇公告
  更差。要治得判断「主体是不是由同构重复块组成」。
- **流式入口拿不到跨页统计。** `cmd/extractor` 一批 256 行里同域页面只有一两页，学不出域名
  级模板——这是接口形态决定的，跨页能力只在爬虫里生效。
- **分栏正文只抽到一栏**（正文被 CSS 拆成多个并列容器时），容器上溯只往祖先方向走，不做
  兄弟合并。
- **抓取失败 1.5%**，robots 拒绝几十个域，没有重试；1000+ 域的冗余足够覆盖。
