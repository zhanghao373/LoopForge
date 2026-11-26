let launchOptions=null;
let launchProjectID='';
let launchRevision=0;
let launchDirectoryRevision=0;
let launchDirectory=null;
let launchSourceRunner=null;
let launchPaths=[];

function sopLaunchMemoryKey(projectId){
  return 'agentflow.sopLaunch.'+String(state.currentUser?.id||0)+'.'+projectId;
}
function readSOPLaunch(projectId){
  try{return JSON.parse(localStorage.getItem(sopLaunchMemoryKey(projectId))||'{}')||{}}
  catch{return {}}
}
function rememberSOPLaunch(projectId,workflowId,payload){
  const saved=readSOPLaunch(projectId),execution=payload.execution;
  saved.workflowId=workflowId;
  if(execution.mode==='runner'){
    if(execution.runnerId!==saved.runnerId){
      delete saved.directory;
      delete saved.directories;
    }
    saved.runnerId=execution.runnerId;
    if(execution.sourcePaths?.length){
      saved.directories=execution.sourcePaths;
      delete saved.directory;
      saved.localSource='directory';
    }else if(payload.repositoryId)saved.localSource='git';
  }
  if(payload.repositoryId)saved.repositoryId=payload.repositoryId;
  try{localStorage.setItem(sopLaunchMemoryKey(projectId),JSON.stringify(saved))}catch{}
}
function switchSOPLaunchSourceRunner(id){
  if(id===launchSourceRunner)return;
  launchSourceRunner=id;
  const saved=readSOPLaunch(launchProjectID);
  launchPaths=id&&id===saved.runnerId?[...(saved.directories||[saved.directory].filter(Boolean))]:[];
  $('#sopLaunchPath').value='';
}
function sopLaunchSelectedPaths(){
  const pending=$('#sopLaunchPath').value.trim().replace(/\/+$/,'');
  return pending&&!launchPaths.includes(pending)?[...launchPaths,pending]:launchPaths.slice();
}
function paintSOPLaunchDirectorySelection(){
  const list=$('#sopLaunchSelectedDirectories');
  list.hidden=!launchPaths.length;
  list.replaceChildren(...launchPaths.map(path=>{
    const row=document.createElement('li');
    const label=document.createElement('span');
    label.textContent=path;
    const remove=document.createElement('button');
    remove.type='button';
    remove.className='button ghost small';
    remove.textContent='移除';
    remove.setAttribute('aria-label','移除 '+path);
    remove.onclick=()=>{
      launchPaths=launchPaths.filter(candidate=>candidate!==path);
      paintSOPLaunch();
    };
    row.append(label,remove);
    return row;
  }));
  const pending=$('#sopLaunchPath').value.trim();
  $('#sopLaunchAddDirectory').disabled=!pending;
  $('#sopLaunchUseDirectory').disabled=!launchDirectory?.selectable||launchPaths.includes(launchDirectory?.path);
}
function addSOPLaunchDirectory(path){
  const selected=path.trim().replace(/\/+$/,'');
  if(selected&&!launchPaths.includes(selected))launchPaths.push(selected);
  if(!launchDirectory||$('#sopLaunchPath').value.trim()===path)$('#sopLaunchPath').value='';
  paintSOPLaunch();
}
function openSOPLaunch(workflowId){
  const enabled=!!state.data?.capabilities?.personalExecutors;
  $('#sopLaunchSettings').hidden=!enabled;
  $('#runWorkflowChoice').onchange=()=>{
    renderSOPLaunchNodes();
    if(enabled)void loadSOPLaunchOptions();
  };
  $('#runInput').oninput=enabled?()=>paintSOPLaunch():null;
  if(!enabled){renderSOPLaunchNodes();return}
  launchProjectID=currentProject()?.id||'';
  launchSourceRunner=null;
  switchSOPLaunchSourceRunner('');
  const select=$('#runWorkflowChoice'),saved=readSOPLaunch(launchProjectID);
  if(!workflowId&&Array.from(select.options).some(option=>option.value===saved.workflowId))select.value=saved.workflowId;
  if(!select.value&&select.options.length)select.selectedIndex=0;
  renderSOPLaunchNodes();
  $('#sopLaunchEnvironment').value='runner';
  $('#sopLaunchDetails').open=false;
  $('#sopLaunchEnvironment').onchange=()=>{closeSOPLaunchDirectories();paintSOPLaunchSources(false);paintSOPLaunch()};
  $('#sopLaunchMachine').onchange=()=>{
    closeSOPLaunchDirectories();
    switchSOPLaunchSourceRunner($('#sopLaunchMachine').value);
    paintSOPLaunch();
  };
  $('#sopLaunchSource').onchange=()=>{closeSOPLaunchDirectories();paintSOPLaunch()};
  $('#sopLaunchPath').oninput=()=>paintSOPLaunch();
  $('#sopLaunchPath').onkeydown=event=>{
    if(event.key==='Enter'){
      event.preventDefault();
      addSOPLaunchDirectory(event.currentTarget.value);
    }
  };
  $('#sopLaunchAddDirectory').onclick=()=>addSOPLaunchDirectory($('#sopLaunchPath').value);
  $('#sopLaunchBrowse').onclick=()=>{
    if($('#sopLaunchDirectoryBrowser').hidden)void loadSOPLaunchDirectories('');
    else{closeSOPLaunchDirectories();paintSOPLaunch()}
  };
  $('#sopLaunchParent').onclick=()=>loadSOPLaunchDirectories(launchDirectory?.parent||'');
  $('#sopLaunchUseDirectory').onclick=()=>{
    if(!launchDirectory?.selectable)return;
    addSOPLaunchDirectory(launchDirectory.path);
  };
  $('#sopLaunchEdit').onclick=()=>{
    const id=select.value;
    if(!id)return;
    $('#runModal').classList.remove('open');
    openSOPDesigner(id);
  };
  void loadSOPLaunchOptions();
}
function renderSOPLaunchNodes(){
  const workflow=(state.data.workflows||[]).find(item=>item.id===$('#runWorkflowChoice').value);
  const nodes=workflow?.tags?.includes('selectable-nodes')?workflow.nodes||[]:[];
  const details=$('#sopLaunchNodes'),presets=$('#sopLaunchNodePresets'),list=$('#sopLaunchNodeList');
  details.hidden=!nodes.length;
  details.open=false;
  presets.replaceChildren();
  list.replaceChildren();
  for(const node of nodes){
    const label=document.createElement('label'),check=document.createElement('input');
    check.type='checkbox';
    check.value=node.id;
    check.dataset.group=node.launchGroup||'';
    check.checked=true;
    check.onchange=updateSOPLaunchNodes;
    label.append(check,document.createTextNode(node.name));
    list.append(label);
  }
  const groups=new Set(nodes.map(node=>node.launchGroup));
  const choices=[['','全部节点'],['frontend','仅前端'],['backend','仅后端']];
  for(const [group,title] of choices){
    if(!nodes.length||(group&&!groups.has(group)))continue;
    const button=document.createElement('button');
    button.type='button';
    button.className='button ghost small';
    button.textContent=title;
    button.onclick=()=>{
      for(const check of list.querySelectorAll('input'))check.checked=!group||!check.dataset.group||check.dataset.group===group;
      updateSOPLaunchNodes();
    };
    presets.append(button);
  }
  paintSOPLaunchNodeCount();
}
function sopLaunchDisabledNodes(){
  return [...$('#sopLaunchNodeList').querySelectorAll('input:not(:checked)')].map(check=>check.value);
}
function paintSOPLaunchNodeCount(){
  const total=$('#sopLaunchNodeList').querySelectorAll('input').length;
  const count=total-sopLaunchDisabledNodes().length;
  $('#sopLaunchNodeSummary').textContent=`本次运行节点（${count} / ${total}）`;
  const status=$('#sopLaunchSelectionStatus');
  status.hidden=!total||count>0;
  status.textContent=status.hidden?'':'至少选择一个 SOP 节点';
  const confirm=$('#confirmRun');
  confirm.disabled=!!confirm.dataset.starting||!$('#runWorkflowChoice').value||!status.hidden;
  return status.hidden;
}
function updateSOPLaunchNodes(){
  if(paintSOPLaunchNodeCount()&&state.data?.capabilities?.personalExecutors)void loadSOPLaunchOptions(true);
  else if(state.data?.capabilities?.personalExecutors){
    ++launchRevision;
    launchOptions=null;
    $('#sopLaunchStatus').textContent='';
  }
}
async function loadSOPLaunchOptions(keepSelection=false){
  const revision=++launchRevision,workflowId=$('#runWorkflowChoice').value;
  const previousMachine=$('#sopLaunchMachine').value,previousSource=$('#sopLaunchSource').value;
  launchOptions=null;
  closeSOPLaunchDirectories();
  $('#sopLaunchStatus').textContent=workflowId?'正在检查执行环境…':'暂无可运行的 SOP';
  $('#sopLaunchCLIList').textContent='';
  $('#sopLaunchSummary').textContent='';
  $('#sopLaunchSource').replaceChildren();
  $('#sopLaunchMachine').replaceChildren();
  $('#sopLaunchDirectoryField').hidden=true;
  $('#confirmRun').disabled=true;
  if(!launchProjectID||!workflowId)return;
  try{
    const query=new URLSearchParams({projectId:launchProjectID,workflowId});
    for(const id of sopLaunchDisabledNodes())query.append('disabledNodeId',id);
    const options=await api('/api/sop-launch/options?'+query,{silent:true});
    if(revision!==launchRevision)return;
    launchOptions=options;
    const saved=readSOPLaunch(launchProjectID),machine=$('#sopLaunchMachine');
    machine.replaceChildren(...options.runners.map(runner=>new Option(runner.name+(runner.error?' · '+runner.error:''),runner.id)));
    const preferred=[keepSelection&&previousMachine,saved.runnerId,options.defaultRunner].find(id=>options.runners.some(runner=>runner.id===id));
    if(preferred)machine.value=preferred;
    switchSOPLaunchSourceRunner(machine.value);
    const cloud=$('#sopLaunchEnvironment').options[1];
    cloud.disabled=!!options.cloud.error;
    cloud.textContent=cloud.disabled?'云端（暂不可用）':'云端';
    if(cloud.disabled)$('#sopLaunchEnvironment').value='runner';
    paintSOPLaunchSources(!keepSelection,previousSource);
    paintSOPLaunch();
  }catch(error){
    if(revision===launchRevision)$('#sopLaunchStatus').textContent=error.message;
  }
}
function paintSOPLaunchSources(first,previousSource){
  const source=$('#sopLaunchSource'),mode=$('#sopLaunchEnvironment').value;
  const saved=readSOPLaunch(launchProjectID),project=currentProject();
  const repos=(state.data.repositories||[]).filter(repo=>repo.projectId===launchProjectID&&repo.cloneUrl);
  const available=[new Option(launchOptions?.requiresSource?'请选择代码来源':'无需代码','')];
  if(mode==='runner')available.push(new Option('本机目录副本','directory'));
  for(const repo of repos)available.push(new Option(repo.name||repo.id,'repo:'+repo.id));
  const rememberedRepo=repos.find(repo=>repo.id===saved.repositoryId)||repos.find(repo=>repo.id===project?.defaultRepositoryId);
  let choice=previousSource??source.value;
  if(first||!available.some(option=>option.value===choice)){
    choice='';
    if(mode==='runner'&&launchOptions?.requiresSource&&sopLaunchSelectedPaths().length)choice='directory';
    else if(rememberedRepo&&launchOptions?.requiresSource)choice='repo:'+rememberedRepo.id;
    else if(mode==='runner'&&launchOptions?.requiresSource)choice='directory';
  }
  source.replaceChildren(...available);
  source.value=choice;
}
function launchMachine(){
  return $('#sopLaunchEnvironment').value==='cloud'
    ?launchOptions?.cloud
    :launchOptions?.runners.find(runner=>runner.id===$('#sopLaunchMachine').value);
}
function paintSOPLaunch(){
  if(!launchOptions)return;
  const mode=$('#sopLaunchEnvironment').value,source=$('#sopLaunchSource'),runner=launchMachine();
  const paths=sopLaunchSelectedPaths(),directory=source.value==='directory';
  const clis=launchOptions.clis.filter(item=>item.name);
  const cliNames=[...new Set(clis.map(item=>item.name))];
  const workflow=(state.data.workflows||[]).find(item=>item.id===$('#runWorkflowChoice').value);
  const sandbox=(state.data.sandboxes||[]).find(item=>item.id===workflow?.sandboxProfileId);
  const sandboxLabel=({native:'原生沙箱',docker:'Docker 沙箱',local:'本机进程'})[sandbox?.driver]||sandbox?.name||'SOP 默认沙箱';
  const sourceLabel=directory?(paths.length===1?paths[0].split('/').at(-1)+' 副本':paths.length>1?paths.length+' 个本机目录副本':'本机目录副本'):source.selectedOptions[0]?.textContent||'';
  $('#sopLaunchDirectoryField').hidden=!directory;
  $('#sopLaunchMachineField').hidden=mode!=='runner'||launchOptions.runners.length<2;
  $('#sopLaunchSummary').textContent=[runner?.name||'未连接本机 Runner',sandboxLabel,sourceLabel,cliNames.join(' / ')].filter(Boolean).join(' · ');
  $('#sopLaunchCLIList').textContent=clis.map(item=>{
    const auth=runner?.auth?.[item.provider];
    return item.node+' '+item.name+(auth==='local'?'（本机登录）':auth==='personal'?'（个人 Key）':'');
  }).join('、')||'此 SOP 只运行脚本或人工确认。';
  let error=runner?.error||(!runner?'请在「执行机器」接入你的 Mac 并设为默认':'');
  if(!error&&(directory&&!paths.length||launchOptions.requiresSource&&!source.value))error='请选择本次使用的代码来源';
  $('#sopLaunchStatus').textContent=error;
  $('#confirmRun').disabled=!!error||!$('#runInput').value.trim()||!!$('#confirmRun').dataset.starting;
  const browse=$('#sopLaunchBrowse');
  browse.disabled=!runner?.id;
  browse.textContent=$('#sopLaunchDirectoryBrowser').hidden?'浏览目录':'收起目录';
  paintSOPLaunchDirectorySelection();
}
function sopLaunchPayload(){
  paintSOPLaunch();
  if(!launchOptions||$('#confirmRun').disabled)return null;
  const mode=$('#sopLaunchEnvironment').value,source=$('#sopLaunchSource').value;
  const execution={mode};
  if(mode==='runner')execution.runnerId=$('#sopLaunchMachine').value;
  if(source==='directory')execution.sourcePaths=sopLaunchSelectedPaths();
  return {execution,...(source.startsWith('repo:')?{repositoryId:source.slice(5)}:{})};
}
function closeSOPLaunchDirectories(){
  launchDirectoryRevision++;
  launchDirectory=null;
  $('#sopLaunchDirectoryBrowser').hidden=true;
}
async function loadSOPLaunchDirectories(path){
  const runnerId=$('#sopLaunchMachine').value;
  if(!runnerId)return;
  const revision=++launchDirectoryRevision,browser=$('#sopLaunchDirectoryBrowser'),list=$('#sopLaunchDirectoryList');
  browser.hidden=false;
  $('#sopLaunchBrowse').textContent='收起目录';
  launchDirectory=null;
  $('#sopLaunchUseDirectory').disabled=true;
  $('#sopLaunchParent').disabled=true;
  $('#sopLaunchDirectoryLabel').textContent='正在读取目录…';
  list.replaceChildren();
  try{
    const directory=await api('/api/sop-launch/runners/'+encodeURIComponent(runnerId)+'/directories?'+new URLSearchParams({path}),{silent:true});
    if(revision!==launchDirectoryRevision)return;
    launchDirectory=directory;
    $('#sopLaunchDirectoryLabel').textContent=directory.path||'可访问位置';
    $('#sopLaunchParent').disabled=!directory.path;
    paintSOPLaunchDirectorySelection();
    list.replaceChildren(...directory.directories.map(path=>{
      const button=document.createElement('button');
      button.type='button';
      button.className='button ghost small';
      button.textContent=directory.path?path.split('/').at(-1):path;
      button.title=path;
      button.onclick=()=>loadSOPLaunchDirectories(path);
      return button;
    }));
  }catch(error){
    if(revision===launchDirectoryRevision)$('#sopLaunchDirectoryLabel').textContent=error.message;
  }
}
