function projectVerifiers(projectId=state.projectFilter){
  return (state.data?.verifiers||[]).filter(verifier=>verifier.projectId===projectId);
}

function verifierAssetPane(a){
  const scriptEnabled=!a.id||!!a.script?.trim();
  return `<div class="asset-tabpanel" data-asset-pane="script" role="tabpanel">
    <label class="asset-switch-row"><span><b>Shell 验收脚本</b></span>
      <input name="scriptEnabled" type="checkbox" class="asset-switch" role="switch" ${scriptEnabled?'checked':''}>
    </label>
    <div data-verifier-script ${scriptEnabled?'':'hidden'}>
      <label class="asset-field-label" for="verifierScript">脚本内容</label>
      <textarea id="verifierScript" name="script" class="asset-editor is-short" rows="9" placeholder="test -s test-report.md">${escapeHTML(a.script||'')}</textarea>
      <label class="asset-field-label" for="verifierScope">检查范围</label>
      <select id="verifierScope" name="executionScope" class="asset-select">
        <option value="run" ${a.executionScope!=='repositories'?'selected':''}>Run 工作区</option>
        <option value="repositories" ${a.executionScope==='repositories'?'selected':''}>每个代码仓库</option>
      </select>
      <label class="asset-field-label" for="verifierTimeout">脚本超时（秒）</label>
      <input id="verifierTimeout" name="timeoutSec" type="number" class="asset-input" min="1" max="3600" value="${Number(a.timeoutSec)||60}">
    </div>
    <label class="asset-switch-row"><span><b>独立模型评审</b></span>
      <input name="reviewEnabled" type="checkbox" class="asset-switch" role="switch" ${a.review?'checked':''}>
    </label>
    <div data-verifier-review ${a.review?'':'hidden'}>
      <label class="asset-field-label" for="verifierExecutor">评审执行器</label>
      <select id="verifierExecutor" name="reviewExecutor" class="asset-select">
        <option value="codex" ${a.review?.executorId!=='claude-code'?'selected':''}>Codex</option>
        <option value="claude-code" ${a.review?.executorId==='claude-code'?'selected':''}>Claude Code</option>
      </select>
      <label class="asset-field-label" for="verifierModel">评审模型</label>
      <input id="verifierModel" name="reviewModel" class="asset-input" list="verifierReviewModels" maxlength="256" value="${escapeHTML(a.review?.model||'')}" placeholder="选择或填写模型 ID">
      <datalist id="verifierReviewModels"></datalist>
      <label class="asset-field-label" for="verifierCriteria">评审标准</label>
      <textarea id="verifierCriteria" name="reviewInstructions" class="asset-editor is-short" rows="4" placeholder="检查哪些验收条件，并说明需要看到什么证据">${escapeHTML(a.review?.instructions||'')}</textarea>
      <label class="asset-field-label" for="verifierArtifacts">发送给模型的产物名称（每行一个）</label>
      <textarea id="verifierArtifacts" name="reviewArtifacts" class="asset-editor is-short" rows="3" placeholder="test-report">${escapeHTML((a.review?.artifactNames||[]).join('\n'))}</textarea>
      <p class="json-help" data-verifier-review-help></p>
    </div>
  </div>`;
}

function bindVerifierAssetForm(form){
  const fields=form.elements;
  const syncStages=()=>{
    const hasScript=fields.scriptEnabled.checked, hasReview=fields.reviewEnabled.checked;
    form.querySelector('[data-verifier-script]').hidden=!hasScript;
    form.querySelector('[data-verifier-review]').hidden=!hasReview;
    fields.script.required=hasScript;
    fields.timeoutSec.required=hasScript;
    fields.timeoutSec.disabled=!hasScript;
    fields.reviewModel.required=hasReview;
    fields.reviewInstructions.required=hasReview;
    fields.scriptEnabled.setCustomValidity(hasScript||hasReview?'':'请至少启用脚本或模型评审');
    form.querySelector('[data-verifier-review-help]').textContent=hasScript
      ?'脚本通过后，模型会读取脚本输出和指定的节点产物。'
      :'模型会读取本次任务和指定的节点产物。';
  };
  const syncModels=()=>{
    const names=fields.reviewExecutor.value==='claude-code'?'claude':'codex';
    form.querySelector('#verifierReviewModels').replaceChildren(
      ...agentModelItems(names,'').filter(item=>item.id&&item.id!==AGENT_CUSTOM_MODEL).map(item=>new Option(item.name||item.id,item.id))
    );
  };
  fields.scriptEnabled.addEventListener('change',syncStages);
  fields.reviewEnabled.addEventListener('change',syncStages);
  fields.reviewExecutor.addEventListener('change',syncModels);
  syncStages();
  syncModels();
  void loadExecutorModelCatalogs().then(()=>{if(form.contains(fields.reviewEnabled))syncModels()});
}

function verifierAssetValues(form,existing){
  const f=form.elements;
  const value={version:existing?.version||0};
  if(f.scriptEnabled.checked)Object.assign(value,{
    script:f.script.value, executionScope:f.executionScope.value, timeoutSec:Number(f.timeoutSec.value)
  });
  if(f.reviewEnabled.checked)value.review={
    executorId:f.reviewExecutor.value, model:f.reviewModel.value.trim(),
    instructions:f.reviewInstructions.value.trim(), artifactNames:lines(f.reviewArtifacts.value)
  };
  return value;
}

function nodeVerifierEditor(n){
  const eligible=!state.workflow?.generic&&!state.workflow?.shared&&!!state.workflow?.projectId;
  const choices=eligible?projectVerifiers(state.workflow.projectId):[];
  const refs=n.verification?.verifiers||[];
  const mode=n.verification==null?'inherit':refs.length?'references':'off';
  const others=id=>new Set(refs.filter(ref=>ref.verifierId!==id).map(ref=>ref.verifierId));
  const failureOptions=[
    ['', '按节点策略'], ['fail','标记失败'], ['human','转人工确认'],
    ...(n.rejectTo?[['reject','打回上游']]:[]), ...(n.type==='agent'?[['loop','本节点自修正']]:[])
  ];
  const rows=refs.map((ref,i)=>{
    const used=others(ref.verifierId);
    const current=choices.find(verifier=>verifier.id===ref.verifierId);
    const options=choices.filter(verifier=>!used.has(verifier.id)).map(verifier=>
      `<option value="${escapeHTML(verifier.id)}" ${ref.verifierId===verifier.id?'selected':''}>${escapeHTML(verifier.name)} · v${Number(verifier.version)||1}</option>`
    ).join('');
    return `<div class="fd-field" data-verifier-ref="${i}">
      <label for="nodeVerifier-${i}">Verifier ${i+1}</label>
      <select id="nodeVerifier-${i}" class="select" data-verifier-id>${current?'':`<option value="${escapeHTML(ref.verifierId)}">${escapeHTML(ref.verifierId)}</option>`}${options}</select>
      <label for="nodeVerifierFailure-${i}">失败后怎么走</label>
      <select id="nodeVerifierFailure-${i}" class="select" data-verifier-failure>${failureOptions.map(([value,label])=>
        `<option value="${value}" ${value===(ref.onFailure||'')?'selected':''}>${label}</option>`).join('')}</select>
      <div><button type="button" class="fd-btn ghost sm" data-verifier-remove>移除</button></div>
    </div>`;
  }).join('');
  const manage=eligible?`<a href="?projectId=${encodeURIComponent(state.workflow.projectId)}#/verifiers" target="_blank" rel="noopener noreferrer">管理本项目的 Verifier</a>`:'';
  return `<div class="fd-field">
    <label for="nodeVerificationMode">额外验收</label>
    <select id="nodeVerificationMode" class="select">
      <option value="inherit" ${mode==='inherit'?'selected':''}>${n.type==='agent'?'继承角色门禁':'默认'}</option>
      <option value="references" ${mode==='references'?'selected':''} ${choices.length||refs.length?'':'disabled'}>引用 Verifier</option>
      <option value="off" ${mode==='off'?'selected':''}>关闭额外门禁</option>
    </select>
    ${manage?`<small>${manage}</small>`:''}
    ${refs.length>1?'<small>同一轮并行校验，全部结束后汇总结果。失败策略优先级为转人工确认、标记失败、打回上游、本节点自修正。</small>':''}
  </div>${rows}${mode==='references'&&choices.length>refs.length?'<button type="button" class="fd-btn ghost sm" data-verifier-add>添加 Verifier</button>':''}`;
}

function bindNodeVerifiers(host,node){
  const mode=host.querySelector('#nodeVerificationMode');
  if(!mode)return;
  const choices=projectVerifiers(state.workflow.projectId);
  const save=change=>{flowHistory.record();change();dirty();renderProperties()};
  const add=()=>{
    const refs=node.verification?.verifiers||[];
    const next=choices.find(v=>!refs.some(ref=>ref.verifierId===v.id));
    if(next)node.verification={verifiers:[...refs,{verifierId:next.id,onFailure:''}]};
  };
  mode.onchange=()=>save(()=>{
    if(mode.value==='inherit')delete node.verification;
    else if(mode.value==='off')node.verification={verifiers:[]};
    else add();
  });
  host.querySelector('[data-verifier-add]')?.addEventListener('click',()=>save(add));
  for(const row of host.querySelectorAll('[data-verifier-ref]')){
    const i=Number(row.dataset.verifierRef),refs=node.verification.verifiers;
    row.querySelector('[data-verifier-id]').onchange=e=>save(()=>{refs[i].verifierId=e.target.value});
    row.querySelector('[data-verifier-failure]').onchange=e=>save(()=>{refs[i].onFailure=e.target.value});
    row.querySelector('[data-verifier-remove]').onclick=()=>save(()=>refs.splice(i,1));
  }
}
