boot().catch(e=>{finishBootLoader();document.body.innerHTML=`<pre style="padding:30px">启动失败：${escapeHTML(e.message)}</pre>`});
