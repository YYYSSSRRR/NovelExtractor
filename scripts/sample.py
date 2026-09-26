#!/usr/bin/env python3
"""从全量爬取产物里抽一份分层样本，作为交付语料。

为什么需要它：交付的 data/pages.jsonl 是 300 页量级的样本，而全量产物有上万页
且不随代码入库。样本是怎么来的必须是**一条命令**，不能是某次会话里敲的一段
一次性代码——上一版的样本恰恰就是这么来的，而且那个脚本原地覆写了
data/pages.jsonl（按字节写、没有 truncate），在覆写边界上把一行切成了碎片，
丢的是行首，truncateTornLine 只认「末尾没有换行」这一种形状，管不到它。
一份完好的全量产物就这么没了。所以这个脚本的硬性约束是：

    只往 stdout 写。绝不打开任何文件来写，绝不接受「原地改写」这种用法。

用法：

    ./bin/crawler -data /tmp/fullrun -seeds seeds/seeds.txt ...   # 先跑一轮全量
    python3 scripts/sample.py --per-host 2 --cap 300 \
        < /tmp/fullrun/pages.jsonl > data/pages.jsonl

注意输入是**全量产物**，而全量产物本身不入库（几十 MB，每次都不同）。
所以这条命令不是「拿仓库里已有的东西跑一遍」就能复现的：要复核交付语料，
得先重跑一轮全量爬取，而站点内容会变，重跑出来的样本不会与现在这份逐行相同。
仓库能保证的只有**规则**——同一份输入两次跑出逐字节相同的输出。当前这份
`data/pages.jsonl` 的 md5 与构成写在 README「语料来源」一节。

抽样规则（两步，各自都是确定性的，不需要随机种子）：

1. 按 host 分组，每组按**等距跨步**取最多 --per-host 条。
   跨步而不是「取正文最长的 N 条」：实测后者会让 300 行全部是
   content_chars>=300 的长页、0 行空正文，样本完全无法反映真实构成
   （全量里空正文占 14.5%）。样本一旦在选择时就偏了，后面所有基于它
   算出来的比例都是错的，而且错得看不出来。

2. 展平后如果超过 --cap，就再对整串做一次等距跨步。
   这是对「每域 ≤ per-host」的子序列取样，不会破坏那条保证。

两步都只依赖输入的顺序与内容，所以同一份输入两次跑出逐字节相同的输出，
输出顺序沿用源文件行序。
"""

import argparse
import json
import sys
from urllib.parse import urlsplit


def host_of(row):
    """取行所属的域。优先用产物里的 host 字段，缺了就按 URL 现算。"""
    h = row.get("host")
    if h:
        return h
    for key in ("final_url", "url"):
        u = row.get(key)
        if u:
            netloc = urlsplit(u).netloc
            if netloc:
                return netloc
    return "(未知)"


def stride_pick(items, n):
    """从 items 里等距取 n 条，保持原有顺序。

    取的是「均匀分布的下标」而不是前 n 条：产物是按抓取完成顺序写的，
    前 n 条往往集中在同一批先跑完的站点上。n >= len(items) 时全取。
    """
    total = len(items)
    if n <= 0:
        return []
    if total <= n:
        return list(items)
    if n == 1:
        return [items[0]]
    # 端点都取到：0 与 total-1 各占一个名额，中间按等距铺开
    idx = sorted({round(i * (total - 1) / (n - 1)) for i in range(n)})
    return [items[i] for i in idx]


def main():
    ap = argparse.ArgumentParser(
        description="按域分层抽样爬取产物（只写 stdout）",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=__doc__,
    )
    ap.add_argument("--in", dest="infile", default="-",
                    help="输入 JSONL，默认从 stdin 读")
    ap.add_argument("--per-host", type=int, default=2,
                    help="每个域最多取几页（默认 2）")
    ap.add_argument("--cap", type=int, default=300,
                    help="输出总页数上限（默认 300），0 表示不限")
    args = ap.parse_args()

    if args.infile == "-":
        fh = sys.stdin
    else:
        # 显式拒绝把输出写回输入：这个脚本只读文件
        fh = open(args.infile, encoding="utf-8")

    rows = []
    bad = 0
    with fh:
        for lineno, line in enumerate(fh, 1):
            line = line.strip()
            if not line:
                continue
            try:
                rows.append(json.loads(line))
            except json.JSONDecodeError as e:
                # 报出来而不是静默丢弃：输入里有坏行本身就是个信号，
                # 上一版那半行碎片如果在这里被报出来，就不会一路带到交付产物里
                bad += 1
                print(f"warn: 第 {lineno} 行不是合法 JSON，已丢弃: {e}", file=sys.stderr)

    groups = {}
    for r in rows:
        groups.setdefault(host_of(r), []).append(r)

    picked = []
    for h in sorted(groups):
        picked.extend(stride_pick(groups[h], args.per_host))

    if args.cap > 0 and len(picked) > args.cap:
        picked = stride_pick(picked, args.cap)

    out = sys.stdout
    for r in picked:
        out.write(json.dumps(r, ensure_ascii=False) + "\n")
    out.flush()

    # 构成报告走 stderr，这样 `> data/pages.jsonl` 出来的文件是纯数据
    chars = sorted(r.get("content_chars", 0) or 0 for r in picked)

    def q(p):
        if not chars:
            return 0
        return chars[min(len(chars) - 1, int(p * len(chars)))]

    print("", file=sys.stderr)
    print(f"输入      {len(rows)} 行（丢弃 {bad} 行）", file=sys.stderr)
    print(f"域        {len(groups)} 个", file=sys.stderr)
    print(f"输出      {len(picked)} 行 / {len({host_of(r) for r in picked})} 个域"
          f"，每域 ≤ {args.per_host} 页，上限 {args.cap}", file=sys.stderr)
    if picked:
        empty = sum(1 for r in picked if not r.get("main_content"))
        print(f"空正文    {empty}（{empty / len(picked):.1%}）", file=sys.stderr)
        print(f"正文长度  min {chars[0]} / p25 {q(.25)} / 中位 {q(.5)}"
              f" / p75 {q(.75)} / max {chars[-1]}", file=sys.stderr)
        print(f"≥300 字   {sum(1 for c in chars if c >= 300)}"
              f"（{sum(1 for c in chars if c >= 300) / len(picked):.1%}）", file=sys.stderr)


if __name__ == "__main__":
    main()
