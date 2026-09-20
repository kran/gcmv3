<template>
    <!-- 点遮罩 / ESC / 右上角 × 都能关; 有未保存修改时 before-close 会先问一句。 -->
    <el-drawer append-to-body v-model="visibleModel" :title="title"
               size="60%" :before-close="requestClose">
        <el-form>
            <field-renderer v-if="def" :fields="def.fields" v-model="form.fields"
                            :node="{ fields: form.fields, expand: refExpand }" :defs="defs"
                            :editing="isEdit" :masked="maskedFields" :readonly="readonlyFields" />
            <p v-if="maskedFields.length" class="fr-hint">
                有 {{ maskedFields.length }} 个字段你看不到（读规则裁掉了）—— 保存不会动它们。
            </p>
            <p v-if="nothingWritable" class="fr-hint">
                这个类型在当前身份下**没有可改的字段**（服务端没有写规则或字段全被裁掉）——
                整份表单只读。
            </p>
        </el-form>
        <template #footer>
            <div style="display:flex;justify-content:flex-end;gap:8px;">
                <el-button @click="requestClose()">取消</el-button>
                <el-button type="primary" :loading="saving" :disabled="nothingWritable"
                    @click="save"><el-icon><Check /></el-icon>保存</el-button>
            </div>
        </template>
    </el-drawer>
</template>
<script>
// NodeEditDialog: 节点新建/编辑共用表单。
// v-model:visible 控制显隐; @changed 保存成功后通知。
export default {
    name: 'NodeEditDialog',
    components: { FieldRenderer: Vue.defineAsyncComponent(() => window.Panel.loadComponent('pages/FieldRenderer.vue')) },
    props: {
        visible: { type: Boolean, required: true },
        node: { type: Object, default: () => ({}) },   // 编辑: 行数据（含 id/type）
        defs: { type: Object, default: () => ({}) },
        typeName: { type: String, default: '' },       // 新建类型（编辑用 node.type）
        presetField: { type: String, default: '' },    // 新建预置字段（如 parent/categories）
        presetValue: { type: Number, default: 0 },     // 预置字段值（如选中的树节点 id）
        presetLabel: { type: String, default: '' },    // 预置字段的显示名（ref 回显 label）
        isEdit: { type: Boolean, default: false },
    },
    emits: ['update:visible', 'changed'],
    data() {
        return {
        refExpand: {},
            form: { revision: 0, fields: {} },
            saving: false, def: null,
            detail: null,   // 刚拉回来的详情（带 masked/editable/expand 三个事实）
            initial: '',   // 加载完成时的表单快照（判断"有没有未保存修改"）
            rebaseline: null, // 打开后重设基线的定时器（控件归一化之后再取）
        }
    },
    computed: {
        // 读规则裁掉的字段（服务端在 masked 里给了名字）—— 表单里不渲染
        // 事实（masked / editable）来自**刚拉回来的详情** —— 列表行里没有它们
        // （读入口只在单节点响应里算这两个事实, 列表要逐项求值太贵）。用列表行算的话
        // 就是"一个字段看着能编、改了却被服务端 422 拒"（真实踩过: published_at）。
        maskedFields() { return ((this.detail || this.node) || {}).masked || [] },
        // 本 actor 在这个节点上不可写的字段 —— 只读展示（服务端算的 editable 之外）
        // 新建时没有节点 ⇒ 没有这组事实（新建的可写集合另说, 由服务端写规则定）
        readonlyFields() {
            if (!this.isEdit) return []
            // 详情还没回来时不下结论（否则会闪一下"全只读"）
            if (!this.detail) return []
            // **缺 editable = 什么都不可写**, 不是"随便写" —— 服务端不给 editable 的
            // 场合正是"这个类型/这个身份没有写规则"（例: staff 后台根本不能改）。
            // 把"缺 facts"当成"没有限制"就是 fail-open: 界面给控件, 一存 403/422
            // （真实踩过: 员工表单里"角色"是勾选框）。
            var editable = this.detail.editable || []
            return ((this.def && this.def.fields) || [])
                .map(function (f) { return f.name })
                .filter(function (name) { return editable.indexOf(name) < 0 })
        },
        // 编辑时一个字段都写不了（没有写规则, 或规则把字段全裁了）
        nothingWritable() {
            if (!this.isEdit || !this.detail) return false
            return (this.detail.editable || []).length === 0
        },
        // 标题在渲染期就会求值, 而 node 只在点开某一行之后才有 ——
        // 调用方（nodes.vue）为了省事把 :is-edit 写成恒 true, 这里必须容忍 node 为空。
        title() {
            if (!this.isEdit) return '新建 ' + (this.typeName || '')
            return this.node ? '编辑 #' + this.node.id : '编辑'
        },
        // v-model:visible 代理 — prop 只读, 内部写走 emit
        visibleModel: {
            get() { return this.visible },
            set(v) { this.$emit('update:visible', v) },
        },
    },
    watch: {
        visible(v) {
            if (!v) return
            clearTimeout(this.rebaseline)
            this.saving = false
            if (this.isEdit) this.loadEdit()
            else this.loadCreate()
        },
    },
    methods: {
        // 快照只含会被提交的东西（声明字段）: refExpand 是回显用的、异步到，
        // 不参与比较，否则"刚打开就显示未保存"。
        formSnapshot() {
            return JSON.stringify({ fields: this.form.fields || {} })
        },
        isDirty() {
            return this.initial !== '' && this.formSnapshot() !== this.initial
        },
        // dirtyDiff 诊断："没动过却提示有修改"时, 到底哪个字段变了。
        // 输出到浏览器控制台（F12）—— 只需要加载时与现在的差异, 不参与业务。
        dirtyDiff() {
            try {
                const before = JSON.parse(this.initial || '{"fields":{}}')
                const after = JSON.parse(this.formSnapshot())
                const diff = {}
                const keys = new Set(Object.keys(before.fields || {}).concat(Object.keys(after.fields || {})))
                keys.forEach((k) => {
                    const was = (before.fields || {})[k]
                    const now = (after.fields || {})[k]
                    if (JSON.stringify(was) !== JSON.stringify(now)) {
                        diff[k] = { 加载时: was, 现在: now }
                    }
                })
                return diff
            } catch (err) {
                return { 诊断失败: String(err) }
            }
        },
        // 抽屉自己的关闭入口（点遮罩 / ESC / ×）走 before-close；底部"取消"和保存成功是
        // 程序化关闭，所以也让它们走同一个检查。
        // done 是 el-drawer 的"继续关闭"回调（before-close 约定）；底部"取消"没有它。
        // 两条路都真的把 visibleModel 置 false，避免依赖某一版 element-plus 的回调语义。
        requestClose(done) {
            var self = this
            var proceed = function () {
                if (typeof done === 'function') done()
                self.visibleModel = false
            }
            if (!this.isDirty()) {
                proceed()
                return
            }
            console.warn('[dirty] 未保存修改的字段差异 node=' + ((this.node && this.node.id) || '?'),
                JSON.parse(JSON.stringify(this.dirtyDiff())))
            ElMessageBox.confirm('表单有未保存的修改，关闭后这些修改会丢失。', '未保存的修改', {
                type: 'warning', confirmButtonText: '放弃修改', cancelButtonText: '继续编辑',
            }).then(proceed).catch(function () {})
        },
        loadCreate() {
            this.def = this.defs[this.typeName] || null
            var defaults = {}
            ;((this.def && this.def.fields) || []).forEach(function (f) {
                if (f.default !== undefined && f.default !== null) defaults[f.name] = structuredClone(f.default)
            })
            this.form = { revision: 0, fields: defaults }
            if (this.presetField && this.presetValue) {
                this.form.fields[this.presetField] = this.presetValue
                // ref 字段显示名（否则只显示裸 id）
                if (this.presetLabel) {
                    // 与 expand 项同形 + 自带 label（refLabel 优先用它）—— 选择器与单元格共用标签
                    var fd = ((this.def || {}).fields || []).find((f) => f.name === this.presetField)
                    this.refExpand[this.presetField] = [{
                        id: this.presetValue,
                        type: (fd && fd.to) || '',
                        label: this.presetLabel,
                    }]
                }
            }
            this.initial = this.formSnapshot()
        },
        loadEdit() {
            var r = this.node
            var type = r.type || this.typeName
            this.def = this.defs[type] || null
            this.detail = null // 先清掉上一个节点的详情: 事实不能串台
            window.$api.node(type, r.id).then((res) => {
                var full = res.node || res
                this.detail = full
                this.form = {
                    revision: full.revision,
                    fields: full.fields || {},
                }
                // 引用标签来自详情响应自带的 expand（与列表接口同形），不再单独请求。
                // 用刚拉回来的详情（full）—— 列表行 r 常常没有 expand，那会让引用字段
                // 初次渲染只剩裸 id（点开下拉才补上）。
                this.refExpand = full.expand || {}
                this.initial = this.formSnapshot()
                // 打开后下一帧再比一次：控件初始化若把某个字段写脏了, 这里立刻能看见
                // （不用等用户点关闭）—— 给我看这一行。
                // 控件（异步组件）加载之后会做格式归一化：时间选择器把同一个时刻换成别的
                // 写法、富文本重新序列化 HTML……。基线必须是"打开后稳定下来的样子"，
                // 否则用户什么都没动也会被判成有修改。先把差异打出来（方便诊断），再重设基线。
                this.rebaseline = setTimeout(() => {
                    if (!this.isDirty()) return
                    console.warn('[dirty] 打开即脏（已把归一化后的状态当作基线）node=' +
                        ((this.node && this.node.id) || '?') + ' type=' + ((this.node && this.node.type) || '?'),
                        JSON.parse(JSON.stringify(this.dirtyDiff())))
                    this.initial = this.formSnapshot()
                }, 150)
            }).catch(() => {})
        },
        save() {
            this.saving = true
            // 只提交类型声明的字段: 存量数据可能带未声明的历史键（v0.8 允许任意字段），
            // 回传它们会被 v0.9 的 Schema 校验拒绝（422 unknown field）；不回传 = 不改动它们。
            // 只提交本 actor 真的可写的字段（服务端 editable 算好的）—— 提交看不见/不可写的
            // 字段会被写规则拒绝（422: hidden / not writable），整次保存白费。
            // 新建时没有节点事实, 提交全部声明字段（超出的由写规则决定, 见 todo 里的 create 列）。
            // 编辑: 只提交服务端说可写的字段（缺 editable ⇒ 空集, 一个都不提交;
            //       真提交了就等着 403/422, 那是界面在撒谎）
            // 新建: null = 不过滤（可写集合只有服务端知道, 由它把关）
            var editable = this.isEdit ? (((this.detail || this.node) || {}).editable || []) : null
            var declared = {}
            var self = this
            ;((this.def && this.def.fields) || []).forEach(function (f) {
                if (editable && editable.indexOf(f.name) < 0) return
                declared[f.name] = (self.form.fields || {})[f.name]
            })
            var body = { fields: declared }
            if (this.isEdit) body.revision = this.form.revision
            var p = this.isEdit
                ? window.$api.updateNode(this.node.type || this.typeName, this.node.id, body)
                : window.$api.createNode(this.typeName, body)
            p.then(() => {
                ElMessage.success('已保存')
                this.initial = this.formSnapshot()   // 保存成功 = 不再有未保存修改
                this.$emit('changed')
                this.visibleModel = false
            }).catch(() => {}).finally(() => { this.saving = false })
        },
    },
}
</script>
<style>
.fr-cell { padding: 4px 8px; border-radius: 4px; background: #f9fafb; border: 1px solid #eee; }
.fr-hint { color: #9ca3af; font-size: 12px; margin: 8px 0 0; }
</style>
