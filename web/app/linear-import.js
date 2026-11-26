function restoreLinearMCPReturn(){
  const url=new URL(location.href);
  const result=url.searchParams.get('linearMCP');
  if(!result)return;
  url.searchParams.delete('linearMCP');
  history.replaceState(history.state,'',url);
  $('#linearImportDialog')?.dispatchEvent(new CustomEvent('linear-oauth-return',{detail:result}));
}

function bindLinearImport(){
  const dialog=$('#linearImportDialog');
  if(!dialog)return;
  const destination=$('#linearImportDestination'),keyInput=$('#linearImportKey'),projectSelect=$('#linearImportProject');
  const filters=$('#linearImportFilters'),statusSelect=$('#linearImportState'),prioritySelect=$('#linearImportPriority'),queryInput=$('#linearImportQuery');
  const load=$('#linearLoadProjects'),preview=$('#linearPreviewIssues'),submit=$('#linearSubmitImport'),close=$('#linearImportClose');
  const status=$('#linearImportStatus'),results=$('#linearImportPreview');
  const mcpPane=$('#linearImportMCPPane'),keyPane=$('#linearImportKeyPane'),connect=$('#linearMCPConnect'),disconnect=$('#linearMCPDisconnect');
  const connectionStatus=$('#linearMCPConnectionStatus');
  const methods=[...dialog.querySelectorAll('[name="linearImportMethod"]')];
  let destinationID='',preferenceKey='',loadedKey='',loadedUserID=0,keyProjects=[],projects=[],busy=false;
  let connection='mcp',enabled=null,connected=false;

  function selectedFilter(){
    return {state:statusSelect.value,priority:prioritySelect.value===''?null:Number(prioritySelect.value),query:queryInput.value.trim()};
  }
  function rememberSelection(){
    if(!projectSelect.value)return;
    try{localStorage.setItem(preferenceKey,JSON.stringify({connection,project:projectSelect.value,...selectedFilter()}))}catch{}
  }
  function showProjects(preferred=''){
    const placeholder=connection==='mcp'&&!connected?'连接 Linear 后可选':'请选择 Linear 项目';
    projectSelect.replaceChildren(new Option(placeholder,''));
    for(const project of projects)projectSelect.add(new Option(project.name,project.id));
    if(projects.some(p=>p.id===preferred))projectSelect.value=preferred;
  }
  function setStatus(message='',error=false){
    status.textContent=message;
    status.hidden=!message;
    status.classList.toggle('is-error',error);
  }
  function clearResults(){
    results.replaceChildren();
    results.hidden=true;
    setStatus();
  }
  function updateControls(){
    const ready=connection==='mcp'?connected:loadedKey&&keyInput.value.trim()===loadedKey;
    const canSelect=ready&&projects.length>0;
    load.disabled=busy||!keyInput.value.trim();
    keyInput.disabled=busy;
    projectSelect.disabled=busy||!canSelect;
    filters.disabled=preview.disabled=submit.disabled=busy||!canSelect||!projectSelect.value;
    for(const method of methods)method.disabled=busy;
    connect.disabled=busy||enabled!==true;
    connect.textContent=connected?'重新授权':'授权连接';
    disconnect.disabled=busy;
    disconnect.hidden=!connected;
    connectionStatus.textContent=enabled===null?'正在检查连接…':enabled===false?'当前部署未启用 Linear MCP':connected?'已连接 Linear':'尚未连接 Linear';
    close.disabled=busy;
  }
  function request(action){
    const body={projectId:destinationID,connection,linearProjectId:projectSelect.value,filter:selectedFilter()};
    if(connection==='key')body.apiKey=keyInput.value.trim();
    return api('/api/sync/linear/'+action,{method:'POST',silent:true,body:JSON.stringify(body)});
  }
  function readPreference(){
    try{return JSON.parse(localStorage.getItem(preferenceKey))||{}}catch{return {}}
  }
  async function perform(message,handler){
    if(busy)return;
    busy=true;
    clearResults();
    setStatus(message);
    updateControls();
    try{await handler()}catch(error){
      if(error.code==='linear_mcp_reconnect'){
        connected=false;
        projects=[];
        showProjects();
      }
      setStatus(error.message,true);
    }finally{busy=false;updateControls()}
  }
  async function loadProjects(){
    const {projects:received}=await request('projects');
    projects=received.slice().sort((a,b)=>a.name.localeCompare(b.name));
    if(connection==='key'){
      loadedKey=keyInput.value.trim();
      keyProjects=projects;
    }
    showProjects(readPreference().project);
    setStatus(projects.length?'':'当前账号可访问的 Linear 项目为空。');
  }
  function useMethod(method,outcome){
    connection=method;
    for(const option of methods)option.checked=option.value===method;
    mcpPane.hidden=method!=='mcp';
    keyPane.hidden=method!=='key';
    projects=method==='key'?keyProjects:[];
    showProjects(readPreference().project);
    clearResults();
    updateControls();
    if(method!=='mcp')return;
    perform('正在读取 Linear 连接…',async()=>{
      const state=await api('/api/sync/linear/mcp',{silent:true});
      enabled=state.enabled;
      connected=state.connected;
      showProjects();
      setStatus();
      if(connected)await loadProjects();
      const notices={
        denied:'Linear 授权未完成。若工作区要求审核，请等待管理员批准后重试。',
        expired:'这次授权已过期，请重新连接。',
        failed:'Linear 连接未保存，请重新授权。',
      };
      if(notices[outcome])setStatus(notices[outcome],true);
    });
  }
  function open(outcome){
    const project=(state.data.projects||[]).find(item=>item.id===state.projectFilter);
    if(!project){toast('请先选择一个 LoopForge 项目');return}
    const userID=state.currentUser?.id||0;
    if(userID!==loadedUserID){
      loadedKey='';keyProjects=[];connected=false;enabled=null;loadedUserID=userID;
    }
    destinationID=project.id;
    preferenceKey='agentflow.linearImport.'+userID+'.'+destinationID;
    const saved=readPreference();
    destination.textContent='导入到 '+(project.name||project.key||project.id);
    keyInput.value=loadedKey;
    statusSelect.value=saved.state||'';
    prioritySelect.value=saved.priority??'';
    queryInput.value=saved.query||'';
    dialog.showModal();
    useMethod(outcome||saved.connection!=='key'?'mcp':'key',outcome);
  }
  $('#importLinearIssues').onclick=()=>open();
  dialog.addEventListener('linear-oauth-return',event=>open(event.detail));
  for(const option of methods)option.onchange=()=>useMethod(option.value);
  connect.onclick=()=>perform('正在打开 Linear 授权…',async()=>{
    const {authorizationUrl}=await api('/api/sync/linear/mcp/connect',{method:'POST',silent:true,body:JSON.stringify({projectId:destinationID})});
    location.assign(authorizationUrl);
  });
  disconnect.onclick=()=>perform('正在断开连接…',async()=>{
    await api('/api/sync/linear/mcp',{method:'DELETE',silent:true});
    connected=false;
    projects=[];
    showProjects();
    setStatus();
  });
  keyInput.oninput=()=>{
    if(keyInput.value.trim()!==loadedKey){
      loadedKey='';
      keyProjects=[];
      projects=[];
      showProjects();
    }
    clearResults();
    updateControls();
  };
  keyInput.onkeydown=event=>{
    if(event.key==='Enter'&&!load.disabled){event.preventDefault();load.click()}
  };
  load.onclick=()=>perform('正在读取 Linear 项目…',loadProjects);
  for(const control of [projectSelect,statusSelect,prioritySelect,queryInput]){
    control.addEventListener('input',()=>{clearResults();rememberSelection();updateControls()});
  }
  preview.onclick=()=>perform('正在查询符合条件的需求…',async()=>{
    const {issues,hasMore}=await request('preview');
    for(const issue of issues){
      const row=document.createElement('li');
      const identifier=document.createElement('strong');
      identifier.textContent=issue.identifier;
      const title=document.createElement('span');
      title.textContent=issue.title;
      const issueState=document.createElement('small');
      issueState.textContent=issue.state?.name||'';
      row.append(identifier,title,issueState);
      results.append(row);
    }
    results.hidden=!issues.length;
    setStatus(issues.length?(hasMore?'预览前 '+issues.length+' 条，导入时会读取全部匹配需求。':'匹配 '+issues.length+' 条需求。'):'没有符合条件的需求。');
  });
  submit.onclick=()=>perform('正在导入符合条件的需求…',async()=>{
    const {created,updated,unchanged}=await request('import');
    if(!created&&!updated&&!unchanged){setStatus('没有符合条件的需求。');return}
    rememberSelection();
    if(refreshIssuesPending)await refreshIssuesPending;
    await refreshIssues();
    dialog.close();
    toast(`Linear 导入完成 · 新增 ${created} · 更新 ${updated} · 无变化 ${unchanged}`,'ok',5000);
  });
  close.onclick=()=>dialog.close();
  dialog.addEventListener('keydown',event=>{if(event.key==='Escape')event.stopPropagation()});
  dialog.addEventListener('cancel',event=>{if(busy)event.preventDefault()});
  dialog.addEventListener('close',()=>{
    keyInput.value='';
    $('#issueHeadingMore')?.focus();
  });
}
