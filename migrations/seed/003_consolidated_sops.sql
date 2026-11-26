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
 '调研现有代码、相关资料和受影响流程，记录证据和约束。梳理需求目标、前后端范围、验收标准及待澄清问题。列出「方案细节」「实施细节」「验证与验收」三部分的小标题与要点，供负责人确认。'),
('generic-requirement-development', 2, 'scope', '确认需求与提纲', 'approval', '', 'none', 'outline', '',
 '确认需求边界、待决问题和提纲覆盖的方案细节、实施细节及验证与验收范围。'),
('generic-requirement-development', 3, 'architecture', '编写技术方案', 'agent', '', 'development', '', 'design',
 '按已确认提纲和仓库现状写技术方案。明确系统边界、接口与数据契约、权限和错误语义、兼容性、联调验收及回滚。说明前后端各自负责的范围，共用文件须明确由哪一端修改。需要技术图时按问题选 PlantUML 图型。方案供两端各自编写开发 Spec。'),
('generic-requirement-development', 4, 'design-accept', '确认技术方案', 'approval', '', 'none', 'architecture', '',
 '确认技术方案中的取舍、前后端边界、接口契约、风险和验收标准。'),
('generic-requirement-development', 5, 'backend-spec', '后端开发 Spec', 'agent', 'backend', 'development', '', 'backend-spec',
 '依据已确认的技术方案列出后端实现范围、涉及的模块和文件、数据与迁移、异常场景、测试和回滚。沿用技术方案约定的接口和共用文件归属，说明依赖或待定项。只写后端开发 Spec。'),
('generic-requirement-development', 6, 'frontend-spec', '前端开发 Spec', 'agent', 'frontend', 'development', '', 'frontend-spec',
 '依据已确认的技术方案列出前端实现范围、影响页面和组件、交互状态、接口对接、模拟数据及验证用例。沿用技术方案约定的接口和共用文件归属。只写前端开发 Spec。'),
('generic-requirement-development', 7, 'backend-implement', '后端开发', 'agent', 'backend', 'development', '', 'backend-implementation',
 '按后端开发 Spec 和已确认的接口契约实现后端改动，核对旧调用方和已有数据，执行适用的构建、单测或契约测试，记录真实结果。只修改本端负责的文件，保留另一端的改动，避免全局格式化、暂存、提交或推送。'),
('generic-requirement-development', 8, 'frontend-implement', '前端开发', 'agent', 'frontend', 'development', '', 'frontend-implementation',
 '按前端开发 Spec 实现界面与交互，依据接口契约独立开发，需要时使用模拟数据验证。记录真实构建和测试结果。只修改本端负责的文件，保留另一端的改动，避免全局格式化、暂存、提交或推送。'),
('generic-requirement-development', 9, 'backend-review', '后端代码评审', 'agent', 'backend', 'development', 'backend-implement', 'backend-review',
 '独立审查后端实际差异、后端开发 Spec 和测试证据，关注接口语义、兼容性、数据和错误场景。记录问题位置和修复建议，存在阻断问题时提交 rework，退回后端开发。'),
('generic-requirement-development', 10, 'frontend-review', '前端代码评审', 'agent', 'frontend', 'development', 'frontend-implement', 'frontend-review',
 '独立审查前端实际差异、前端开发 Spec 和测试证据，关注交互状态、可访问性、接口对接和回归。记录问题位置和修复建议，存在阻断问题时提交 rework，退回前端开发。'),
('generic-requirement-development', 11, 'qa', '自测与联调', 'agent', '', 'testing', '', 'test-report',
 '结合本次启用的开发与评审结果，执行真实项目测试、构建和可用的端到端场景。两端都有改动时验证真实接口对接。任务要求效果优化且提供评估入口时，同口径测量基线和候选。记录命令、退出码、结果和未覆盖项。存在阻断失败时提交 fail，并说明需要返工的环节。'),
('generic-requirement-development', 12, 'accept', '人工验收', 'approval', '', 'none', 'qa', '',
 '对照任务范围，核对实际交付、已启用的两端评审、自测联调结果和剩余问题。'),
('generic-requirement-development', 13, 'delivery', '交付汇总', 'agent', '', 'none', '', 'delivery',
 '汇总工作区实际变更、方案、测试证据、剩余限制及交付文件。有代码变更时归档 change.patch，覆盖工作区本任务的变更和新增文件。适用时起草 MR/PR 正文，引用已存在的真实链接。'),

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
       JSON_OBJECT('agentId', CONCAT(workflow_id, '-', node_id),
                   'artifactOutputs', JSON_ARRAY(JSON_OBJECT('name', artifact, 'path', CONCAT(artifact, '.md'),
                                                            'mediaType', 'text/markdown', 'required', TRUE)))),
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
      WHEN 'frontend-spec' THEN 'design-accept'
      WHEN 'backend-implement' THEN 'backend-spec'
      WHEN 'frontend-implement' THEN 'frontend-spec'
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
    'id', CONCAT(workflow_id, '-', node_id), 'name', name, 'version', 1, 'executorId', 'codex',
    'prompt', CONCAT(prompt, CHAR(10), CHAR(10), '阅读仓库约定、任务和可用的上游产物。若本轮跳过上游节点，使用已有材料，缺少必要证据时说明。不要伪造结果、泄露凭据或执行未经授权的外部写入。写入 ', artifact, '.md。', CHAR(10), CHAR(10), '任务：{{input}}', CHAR(10), '仓库：{{repository}}')
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
    '调研、澄清与技术方案确认，前后端并行编写开发 Spec、实现和评审，再自测联调、验收交付。' AS description,
    3 AS version, 'backend' AS lane, '需求开发' AS metric_category, JSON_ARRAY() AS requirements
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
SET current_sop.payload = CAST(replacement.payload AS JSON), current_sop.updated_at = UTC_TIMESTAMP()
WHERE (current_sop.id = 'generic-requirement-development' AND (
    (JSON_EXTRACT(current_sop.payload, '$.Workflow.revision') = 1
     AND JSON_CONTAINS(JSON_EXTRACT(current_sop.payload, '$.Workflow.nodes'), JSON_OBJECT('id', 'backend-design')))
    OR
    (JSON_EXTRACT(current_sop.payload, '$.Workflow.revision') = 2
     AND JSON_CONTAINS(JSON_EXTRACT(current_sop.payload, '$.Workflow.nodes'), JSON_OBJECT('id', 'detailed-design')))
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
