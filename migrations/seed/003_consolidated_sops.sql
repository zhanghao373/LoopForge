-- Install the two current blueprints. Former blueprints remain resolvable
-- by ID for projects, roles and runs already bound to them.
SET NAMES utf8mb4;
START TRANSACTION;

CREATE TEMPORARY TABLE consolidated_sop_steps (
  workflow_id VARCHAR(64) NOT NULL,
  seq INT NOT NULL,
  node_id VARCHAR(48) NOT NULL,
  name VARCHAR(64) NOT NULL,
  node_type VARCHAR(16) NOT NULL,
  launch_group VARCHAR(16) NOT NULL,
  metric_stage VARCHAR(16) NOT NULL,
  reject_to VARCHAR(48) NOT NULL,
  artifact VARCHAR(64) NOT NULL,
  prompt TEXT NOT NULL,
  PRIMARY KEY (workflow_id, seq)
);

INSERT INTO consolidated_sop_steps VALUES
('generic-requirement-development', 1, 'outline', '调研、需求澄清与提纲', 'agent', '', 'clarification', '', 'outline',
 '调研现有代码、相关资料和受影响流程，区分已确认事实、假设和待决定事项，并注明证据出处。为每项验收条件写出可观察结果。列出「方案细节」「实施细节」「验证与验收」的小标题与要点。按对实现的影响列出待决问题及可选方案，交给负责人确认。'),
('generic-requirement-development', 2, 'scope', '确认需求与提纲', 'approval', '', 'none', 'outline', '',
 '确认需求边界、待决问题和提纲覆盖的方案细节、实施细节及验证与验收范围。'),
('generic-requirement-development', 3, 'architecture', '编写技术方案', 'agent', '', 'development', '', 'design',
 '从已确认提纲、验收条件和现有代码调用链出发，确定系统边界、前后端职责、接口与数据契约、权限、失败和兼容语义、迁移与回滚。关键决策说明取舍及影响。给出实现依赖、先后顺序、可并行范围和共用文件归属，让每项验收条件都能对应验证方法。需要技术图时按问题选 PlantUML 图型。影响方案的决定缺失时提交 human。'),
('generic-requirement-development', 4, 'design-accept', '确认技术方案', 'approval', '', 'none', 'architecture', '',
 '确认技术方案中的取舍、前后端边界、接口契约、风险和验收标准。'),
('generic-requirement-development', 5, 'development-spec', '前后端开发 Spec', 'agent', '', 'development', '', 'interface-contract',
 '以已确认的技术方案、任务和源码为依据。若从此节点开始，采用任务中已确认的方案或明确的实现约束与验收条件。影响实现或接口对齐的决定缺失时提交 human，列出需确认的事项。写 interface-contract.md，统一记录接口、请求与响应示例、权限、错误、兼容性、联调验收和共用文件归属。写 backend-spec.md，列出后端模块、实现顺序、数据迁移、异常场景、测试和回滚。写 frontend-spec.md，列出页面、组件、实现顺序、加载与空态和错误态、模拟数据及验证。两份 Spec 都引用同一契约，列出各自负责的文件或目录，并把每项职责对应到验收条件和检查方法。核对并行改动没有文件归属冲突。只启用一端时仍写齐三份文件，另一端只记录本轮边界和已有依赖。本节点只产出 Spec。'),
('generic-requirement-development', 7, 'backend-implement', '后端开发', 'agent', 'backend', 'development', '', '',
 '读取 backend-spec.md 和 interface-contract.md，在本次运行的工作区按本端文件归属实现。复用现有实现，检查旧调用方、已有数据及错误和回滚路径，运行适用的构建、单测和契约测试。实现前后核对实际差异，保留另一端的并行改动，避免全局格式化、暂存、提交或推送。必须改变共享契约或文件归属、缺少关键资料时提交 human。收到返工意见时定位问题、修正并重跑相关检查。节点结果列出改动、对应验收项、实际执行的命令、结果和未验证项。'),
('generic-requirement-development', 8, 'frontend-implement', '前端开发', 'agent', 'frontend', 'development', '', '',
 '读取 frontend-spec.md 和 interface-contract.md，在本次运行的工作区按本端文件归属实现。复用现有组件及交互模式，核对加载、空态、错误和权限场景，必要时用符合契约的模拟数据自测，运行适用的构建和测试。实现前后核对实际差异，保留另一端的并行改动，避免全局格式化、暂存、提交或推送。必须改变共享契约或文件归属、缺少关键资料时提交 human。收到返工意见时定位问题、修正并重跑相关检查。节点结果列出改动、对应验收项、实际执行的命令、结果和未验证项。'),
('generic-requirement-development', 9, 'backend-review', '后端代码评审', 'agent', 'backend', 'development', 'backend-implement', 'backend-review',
 '独立检查工作区实际差异，按 backend-spec.md、interface-contract.md 和验收条件核对本端实现、文件归属、接口兼容、数据、权限与错误场景。核对测试是否覆盖关键路径，必要时运行可复现的检查。每条发现写明文件及位置、触发条件、影响和修复方向，区分阻断问题与改进建议。阻断问题提交 rework，退回后端开发。验证所必需的资料或环境缺失时提交 human。评审阶段只记录结论。'),
('generic-requirement-development', 10, 'frontend-review', '前端代码评审', 'agent', 'frontend', 'development', 'frontend-implement', 'frontend-review',
 '独立检查工作区实际差异，按 frontend-spec.md、interface-contract.md 和验收条件核对本端实现、文件归属、交互状态、可访问性、接口对接和回归。核对测试是否覆盖关键路径，必要时运行可复现的检查。每条发现写明文件及位置、触发条件、影响和修复方向，区分阻断问题与改进建议。阻断问题提交 rework，退回前端开发。验证所必需的资料或环境缺失时提交 human。评审阶段只记录结论。'),
('generic-requirement-development', 11, 'qa', '自测与联调', 'agent', '', 'testing', '', 'test-report',
 '对照实际改动、评审结果和验收条件，先确认仓库已有测试入口和运行环境，再执行适用的构建、测试和端到端检查。覆盖主路径、关键失败路径和受影响的旧行为。两端都有改动时验证真实接口对接，并写明哪些结果来自模拟数据。任务要求效果优化且提供评估入口时，同口径测量基线和候选。逐项记录通过、失败、受阻或未执行及原因，附实际命令、退出码和关键输出。阻断失败时提交 fail 并指出应返工的环节。缺少必要环境或资料时提交 human，说明所需条件。'),
('generic-requirement-development', 12, 'accept', '人工验收', 'approval', '', 'none', 'qa', '',
 '对照任务范围，核对实际交付、已启用的两端评审、自测联调结果和剩余问题。'),
('generic-requirement-development', 13, 'delivery', '交付汇总', 'agent', '', 'none', '', 'delivery',
 '核对工作区最终差异、方案、评审结论、联调证据和人工验收结果，逐项汇总交付结果、证据位置与剩余限制。涉及发布风险时注明回滚办法。仅记录本次验证过且可复用的技术决策或踩坑，标出适用条件和证据。有代码变更时归档 change.patch，覆盖工作区本任务的变更和新增文件。适用时起草 MR/PR 正文，引用已存在的真实链接。'),

('generic-defect-repair', 1, 'reproduce', '收集信息与复现', 'agent', '', 'clarification', '', 'reproduction',
 '从任务和相关源码提取预期、实际表现、版本、环境、最小复现步骤，执行能安全复现的检查。记录真实输出；无法复现时写明缺失信息和已尝试的检查。'),
('generic-defect-repair', 2, 'diagnose', '定位根因', 'agent', '', 'development', '', 'root-cause',
 '根据复现证据与代码调用链定位根因和影响面，说明证据、尚未验证的假设、最小修改和验证方法。'),
('generic-defect-repair', 3, 'fix', '最小修复', 'agent', '', 'development', '', 'implementation',
 '根据根因修复工作区代码。按需要添加能捕获本次回归的测试，记录修复原因、范围和回滚方法，执行针对性验证。不提交或推送。'),
('generic-defect-repair', 4, 'review', '独立修复评审', 'agent', '', 'development', 'fix', 'review',
 '独立核对根因证据、实际差异、修复范围和边界，一并审查相关测试。记录具体问题与位置，有阻断问题时提交 rework。'),
('generic-defect-repair', 5, 'regression', '复测与回归验证', 'agent', '', 'testing', 'fix', 'test-report',
 '复测原始场景并执行受影响模块的回归检查。记录真实命令、退出码和结果。当原始场景无法复现时写明证据缺口；阻断失败时提交 rework。'),
('generic-defect-repair', 6, 'accept', '人工确认修复', 'approval', '', 'none', 'fix', '',
 '核对根因、修复差异、复测及回归证据。'),
('generic-defect-repair', 7, 'delivery', '修复交付', 'agent', '', 'none', '', 'delivery',
 '汇总症状、根因、修复、复测与回归证据、风险和回滚点，列出实际交付文件。有代码变更时归档 change.patch，覆盖工作区本任务的变更和新增文件。适用时起草 MR/PR 正文，引用已存在的真实链接。');

CREATE TEMPORARY TABLE consolidated_sop_nodes AS
WITH node_rows AS (
  SELECT workflow_id, seq, JSON_MERGE_PATCH(
    JSON_OBJECT('id', node_id, 'name', name, 'type', node_type,
                'metricStage', metric_stage, 'maxAttempts', 1, 'maxReworks', 2,
                'position', JSON_OBJECT(
                  'x', 100 + 245 * CASE
                    WHEN workflow_id = 'generic-defect-repair' OR seq < 5 THEN seq - 1
                    WHEN seq <= 10 THEN 4 + ((seq - 5) DIV 2)
                    ELSE seq - 4
                  END,
                  'y', CASE launch_group WHEN 'backend' THEN 50 WHEN 'frontend' THEN 250 ELSE 150 END)),
    IF(launch_group = '', JSON_OBJECT(), JSON_OBJECT('launchGroup', launch_group)),
    IF(reject_to = '', JSON_OBJECT(), JSON_OBJECT('rejectTo', reject_to, 'onFailure', 'reject')),
    IF(node_type = 'approval', JSON_OBJECT('taskPrompt', prompt),
       JSON_OBJECT('agentId', CONCAT(workflow_id, '-', node_id))),
    CASE
      WHEN artifact = '' THEN JSON_OBJECT()
      WHEN node_id = 'development-spec' THEN JSON_OBJECT('artifactOutputs', JSON_ARRAY(
        JSON_OBJECT('name', artifact, 'path', CONCAT(artifact, '.md'), 'mediaType', 'text/markdown', 'required', TRUE),
        JSON_OBJECT('name', 'backend-spec', 'path', 'backend-spec.md', 'mediaType', 'text/markdown', 'required', TRUE),
        JSON_OBJECT('name', 'frontend-spec', 'path', 'frontend-spec.md', 'mediaType', 'text/markdown', 'required', TRUE)))
      ELSE JSON_OBJECT('artifactOutputs', JSON_ARRAY(
        JSON_OBJECT('name', artifact, 'path', CONCAT(artifact, '.md'), 'mediaType', 'text/markdown', 'required', TRUE)))
    END,
    IF(node_id = 'delivery', JSON_OBJECT('artifactOutputs',
      JSON_ARRAY(JSON_OBJECT('name', artifact, 'path', CONCAT(artifact, '.md'), 'mediaType', 'text/markdown', 'required', TRUE),
                 JSON_OBJECT('name', 'change', 'path', 'change.patch', 'mediaType', 'text/plain', 'required', FALSE))), JSON_OBJECT())
  ) AS node
  FROM consolidated_sop_steps
), node_lists AS (
  SELECT workflow_id, seq, JSON_ARRAYAGG(node) OVER stage_order AS nodes
  FROM node_rows
  WINDOW stage_order AS (PARTITION BY workflow_id ORDER BY seq ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING)
)
SELECT workflow_id, nodes FROM node_lists WHERE seq = 1;

CREATE TEMPORARY TABLE consolidated_sop_edges AS
WITH ordered_steps AS (
  SELECT workflow_id, seq, node_id, LAG(node_id) OVER (PARTITION BY workflow_id ORDER BY seq) AS previous_node
  FROM consolidated_sop_steps
), edge_rows AS (
  SELECT workflow_id, seq, node_id,
    CASE node_id
      WHEN 'backend-implement' THEN 'development-spec'
      WHEN 'frontend-implement' THEN 'development-spec'
      WHEN 'backend-review' THEN 'backend-implement'
      WHEN 'frontend-review' THEN 'frontend-implement'
      ELSE previous_node
    END AS predecessor
  FROM ordered_steps WHERE previous_node IS NOT NULL
  UNION ALL
  SELECT 'generic-requirement-development', 100, 'qa', 'backend-review'
), edge_lists AS (
  SELECT workflow_id, seq, JSON_ARRAYAGG(JSON_OBJECT('id', CONCAT('e', seq), 'source', predecessor, 'target', node_id, 'when', 'success')) OVER stage_order AS edges
  FROM edge_rows
  WINDOW stage_order AS (PARTITION BY workflow_id ORDER BY seq ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING)
)
SELECT workflow_id, edges FROM edge_lists WHERE seq = 2;

CREATE TEMPORARY TABLE consolidated_sop_agents AS
WITH agent_rows AS (
  SELECT workflow_id, seq, JSON_OBJECT(
    'id', CONCAT(workflow_id, '-', node_id), 'name', name,
    'version', IF(workflow_id = 'generic-requirement-development', 2, 1), 'executorId', 'codex',
    'prompt', CONCAT(prompt, CHAR(10), CHAR(10), '阅读仓库约定、任务和可用的上游产物。若本轮跳过上游节点，使用已有材料，缺少必要证据时说明。不要伪造结果、泄露凭据或执行未经授权的外部写入。', IF(artifact = '' OR node_id = 'development-spec', '', CONCAT('写入 ', artifact, '.md。')), CHAR(10), CHAR(10), '任务：{{input}}', CHAR(10), '仓库：{{repository}}')
  ) AS agent
  FROM consolidated_sop_steps WHERE node_type = 'agent'
), agent_lists AS (
  SELECT workflow_id, seq, JSON_ARRAYAGG(agent) OVER stage_order AS agents
  FROM agent_rows
  WINDOW stage_order AS (PARTITION BY workflow_id ORDER BY seq ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING)
)
SELECT workflow_id, agents FROM agent_lists WHERE seq = 1;

CREATE TEMPORARY TABLE consolidated_sop_definitions AS
WITH templates AS (
  SELECT 'generic-requirement-development' AS id, '需求开发 SOP' AS name,
    '调研、澄清与技术方案确认，统一生成前后端开发 Spec，随后并行实现和评审，再自测联调、验收交付。' AS description,
    5 AS version, 'backend' AS lane, '需求开发' AS metric_category, JSON_ARRAY() AS requirements
  UNION ALL SELECT 'generic-defect-repair', '缺陷修复 SOP',
    '复现、定位、最小修复、独立评审、回归验证和验收。',
    2, 'repair', 'Bug 修复', JSON_ARRAY(JSON_OBJECT('key', 'source-code', 'label', '待修项目代码'))
)
SELECT t.id, t.name,
       JSON_OBJECT('Workflow', JSON_OBJECT('id', t.id, 'name', t.name, 'description', t.description,
         'version', t.version, 'revision', t.version, 'publishedRevision', t.version, 'status', 'published',
         'sandboxProfileId', 'generic-docker', 'generic', TRUE, 'owner', 'LoopForge',
         'sopLane', t.lane, 'metricCategory', t.metric_category,
         'tags', JSON_ARRAY('public', 'selectable-nodes'), 'requirements', CAST(t.requirements AS JSON),
         'nodes', CAST(n.nodes AS JSON), 'edges', CAST(e.edges AS JSON)), 'Agents', CAST(a.agents AS JSON)) AS payload
FROM templates t
JOIN consolidated_sop_nodes n ON n.workflow_id = t.id
JOIN consolidated_sop_edges e ON e.workflow_id = t.id
JOIN consolidated_sop_agents a ON a.workflow_id = t.id;

INSERT IGNORE INTO generic_templates (id, name, payload, updated_at)
SELECT id, name, CAST(payload AS JSON), UTC_TIMESTAMP() FROM consolidated_sop_definitions;

-- Upgrade the shipped consolidated blueprints when their prior revisions are present.
UPDATE generic_templates current_sop
JOIN consolidated_sop_definitions replacement ON replacement.id COLLATE utf8mb4_unicode_ci = current_sop.id
SET current_sop.payload = CASE
    WHEN current_sop.id = 'generic-requirement-development'
         AND JSON_CONTAINS(JSON_EXTRACT(current_sop.payload, '$.Agents'), JSON_ARRAY(
           JSON_OBJECT('id', 'generic-requirement-development-backend-spec'),
           JSON_OBJECT('id', 'generic-requirement-development-frontend-spec'))) THEN
      -- Keep the former Spec roles resolvable for runs carrying the old graph.
      JSON_ARRAY_APPEND(CAST(replacement.payload AS JSON),
        '$.Agents', JSON_EXTRACT(current_sop.payload, REPLACE(JSON_UNQUOTE(JSON_SEARCH(current_sop.payload, 'one', 'generic-requirement-development-backend-spec', NULL, '$.Agents[*].id')), '.id', '')),
        '$.Agents', JSON_EXTRACT(current_sop.payload, REPLACE(JSON_UNQUOTE(JSON_SEARCH(current_sop.payload, 'one', 'generic-requirement-development-frontend-spec', NULL, '$.Agents[*].id')), '.id', '')))
    ELSE CAST(replacement.payload AS JSON)
  END,
  current_sop.updated_at = UTC_TIMESTAMP()
WHERE (current_sop.id = 'generic-requirement-development' AND (
    (JSON_EXTRACT(current_sop.payload, '$.Workflow.revision') = 1
     AND JSON_CONTAINS(JSON_EXTRACT(current_sop.payload, '$.Workflow.nodes'), JSON_OBJECT('id', 'backend-design')))
    OR
    (JSON_EXTRACT(current_sop.payload, '$.Workflow.revision') = 2
     AND JSON_CONTAINS(JSON_EXTRACT(current_sop.payload, '$.Workflow.nodes'), JSON_OBJECT('id', 'detailed-design')))
    OR
    (JSON_EXTRACT(current_sop.payload, '$.Workflow.revision') = 3
     AND JSON_EXTRACT(current_sop.payload, '$.Workflow.publishedRevision') = 3
     AND JSON_CONTAINS(JSON_EXTRACT(current_sop.payload, '$.Workflow.nodes'), JSON_ARRAY(JSON_OBJECT('id', 'backend-spec'), JSON_OBJECT('id', 'frontend-spec')))
     AND JSON_CONTAINS(JSON_EXTRACT(current_sop.payload, '$.Agents'), JSON_ARRAY(JSON_OBJECT('id', 'generic-requirement-development-backend-spec'), JSON_OBJECT('id', 'generic-requirement-development-frontend-spec')))
     AND NOT EXISTS (SELECT 1 FROM JSON_TABLE(current_sop.payload, '$.Agents[*]' COLUMNS (role_version INT PATH '$.version')) AS roles WHERE roles.role_version > 1))
    OR
    (JSON_EXTRACT(current_sop.payload, '$.Workflow.revision') = 4
     AND JSON_EXTRACT(current_sop.payload, '$.Workflow.publishedRevision') = 4
     AND JSON_CONTAINS(JSON_EXTRACT(current_sop.payload, '$.Workflow.nodes'), JSON_OBJECT('id', 'development-spec'))
     AND NOT EXISTS (SELECT 1 FROM JSON_TABLE(current_sop.payload, '$.Agents[*]' COLUMNS (role_version INT PATH '$.version')) AS roles WHERE roles.role_version > 1))
  )) OR (
    current_sop.id = 'generic-defect-repair'
    AND JSON_EXTRACT(current_sop.payload, '$.Workflow.revision') = 1
    AND JSON_EXTRACT(current_sop.payload, '$.Agents[*].executorId') =
        JSON_ARRAY('codebuddy', 'codebuddy', 'codebuddy', 'codebuddy', 'codebuddy', 'codebuddy')
  );

UPDATE generic_templates
SET payload = JSON_SET(payload, '$.Workflow.status', 'archived'), updated_at = UTC_TIMESTAMP()
WHERE id IN ('generic-backend-delivery', 'generic-frontend-delivery', 'generic-product-delivery', 'issue-fix-production')
  AND JSON_UNQUOTE(JSON_EXTRACT(payload, '$.Workflow.status')) <> 'archived';

DROP TEMPORARY TABLE consolidated_sop_steps, consolidated_sop_nodes, consolidated_sop_edges, consolidated_sop_agents, consolidated_sop_definitions;
COMMIT;
