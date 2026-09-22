# 待办（框架）

## 有人用再补

- [x] **settings 插件有消费方了**：lizhiqi 接上（首页简介 + 客户图），并配了
      `tools/import-settings`（v2 的 settings 表 → 插件的表，值原样）。
      随手加的 `richtext` 类型（声明说实话，面板按多行文本框编辑）。
- [ ] 插件面板不在后台闸门扫描范围：`plugin/*/web/*.vue`（backup / settings 的面板）
      没被 `web/admin/_tools/check.js` 编译检查 ⇒ 面板里的语法错误要等浏览器里才炸。
- [ ] 搜索没有高亮（v2 的 highlight 插件已废弃）。要做就给 `web/render` 加一个 mark/highlight 内置。

## 量大了再说

- [ ] 评论 `total` 有 10,000 上限（`CountNodes` 的 `DefaultCountLimit`）⇒ 超过就是下限，
      前端的"还有没有更多"会提前收工。要精确就给接口加 `has_more`（多取一行探测）。
- [ ] 搜索的权限下推（CTE）：现在是"边翻边筛"（FTS 命中 → 读入口回读），一页可能凑不满
      （有 `roundLimit` 兜）。实测：常见词 1s 支撑约 20 万篇、整句短语约 10 万篇；
      顺手记一个白捡的：单 token 查询时短语那一趟是白跑的（常见词延迟可砍半）。
- [ ] 索引体积 ≈ 正文 6 倍（bigram 的固有代价）；`detail=none` 能省空间但会牺牲短语查询
      —— 而短语是排序质量的一半，别乱关。

## 已完成（留着备查）

- [x] `legacy-migrate` 那句写死的"settings（空的）"改成**真实行数** —— 它是假的标签，
      碰上有数据的库会把人骗过去（lizhiqi 首页空白就是这么来的）。

- [x] `web.HostMux`（多站分发，按 Host 头）+ 重复 Host 报错（v2 是静默覆盖）。
- [x] `site.yaml` 的 `fields:`（站点自己的配置）+ `Site.Fields()`（返回副本）。
- [x] `core.NodeQuery.Expand`（nil = 全展开 ⇒ 老调用方不变；列路径 = 只展开这些）——
      评论列表不再把每行引用的整篇文章拖进响应。
- [x] 评论插件（评论 = 节点，策略管可见性；插件只管反垃圾与端点）。
- [x] 搜索插件改成"永远 OR + bm25 + 短语 RRF"（不再三级放宽选一级）。
- [x] 后台列表检索框 / 缩略图 imgproc 参数 / 树视图不显示检索框（43 条自检闸门）。
