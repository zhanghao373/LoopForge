const form = document.getElementById('composer');
const input = document.getElementById('message');
const attachment = document.getElementById('document');
const send = document.getElementById('send');
const delegate = document.getElementById('delegate');
const ticketTarget = document.getElementById('ticket-target');
const status = document.getElementById('status');
const messages = document.getElementById('messages');
const endpoint = '/api/agent-chat/messages';
const threads = document.getElementById('threads');
const thread = new URLSearchParams(location.search).get('thread') || '';
const threadQuery = thread ? `?thread=${encodeURIComponent(thread)}` : '';
const inputLimit = 4000;
let retry;
let poll;
let refreshID = 0;
let submitting = false;
let selectedTicket = '';

function selectTicket(id, text = '') {
  selectedTicket = id;
  ticketTarget.hidden = !id;
  document.getElementById('ticket-label').textContent = id ? `追问任务 · ${text}` : '';
  updateSend();
  input.focus();
}

function writeTurn(container, label, text) {
  const bubble = document.createElement('p');
  bubble.className = 'message';
  const heading = document.createElement('strong');
  heading.className = 'message-label';
  heading.textContent = label;
  bubble.append(heading, document.createTextNode(text));
  container.append(bubble);
}

async function request(options, url = endpoint + threadQuery) {
  const response = await fetch(url, {...options, cache: 'no-store'});
  if (response.status === 401) {
    location.assign('/login.html?returnUrl=' + encodeURIComponent('/agent-chat' + threadQuery));
    throw new Error('请先登录');
  }
  const data = await response.json();
  if (!response.ok) throw new Error(data.error || '对话服务暂不可用');
  return data;
}

async function refreshThreads() {
  const recent = await request({}, '/api/agent-chat/threads');
  const entries = [{id: '', title: '默认对话'}, ...recent];
  if (thread && !recent.some(item => item.id === thread)) {
    entries.push({id: thread, title: '当前对话'});
  }
  threads.replaceChildren(...entries.map(item => {
    const link = document.createElement('a');
    link.href = '/agent-chat' + (item.id ? `?thread=${encodeURIComponent(item.id)}` : '');
    link.textContent = item.title;
    if (item.id === thread) link.setAttribute('aria-current', 'page');
    return link;
  }));
  threads.querySelector('[aria-current="page"]')?.scrollIntoView({block: 'nearest'});
}

async function stopTurn(id, button) {
  button.disabled = true;
  try {
    await request({method: 'POST'}, `${endpoint}/${encodeURIComponent(id)}/cancel${threadQuery}`);
    await refresh();
  } catch (error) {
    status.textContent = error.message;
    button.disabled = false;
  }
}

async function refresh() {
  const id = ++refreshID;
  clearTimeout(poll);
  try {
    const turns = await request();
    if (id !== refreshID) return;
    const pending = turns.some(turn => ['queued', 'routing', 'running', 'reconnecting', 'stopping'].includes(turn.state));
    const snapshot = JSON.stringify(turns);
    if (snapshot !== refresh.snapshot) {
      const rows = document.createDocumentFragment();
      for (const turn of turns) {
        const row = document.createElement('article');
        row.className = 'turn';
        const label = ['你'];
        if (turn.ticketId) label.push('任务');
        if (turn.hasDocument) label.push('附文本');
        writeTurn(row, label.join(' · '), turn.text);
        const placeholder = {queued: '正在排队…', routing: '正在判断去向…', running: '正在处理…', reconnecting: '正在重新连接…',
          stopping: '正在停止…', canceled: '已停止'}[turn.state] || '';
        writeTurn(row, '助手', turn.reply || placeholder);
        const actions = document.createElement('div');
        actions.className = 'turn-actions';
        if (['queued', 'routing', 'running', 'reconnecting'].includes(turn.state)) {
          const stop = document.createElement('button');
          stop.className = 'quiet-button';
          stop.type = 'button';
          stop.textContent = '停止';
          stop.addEventListener('click', () => stopTurn(turn.id, stop));
          actions.append(stop);
        }
        if (turn.ticketId) {
          const follow = document.createElement('button');
          follow.className = 'quiet-button';
          follow.type = 'button';
          follow.textContent = '追问任务';
          follow.addEventListener('click', () => selectTicket(turn.ticketId, turn.text));
          actions.append(follow);
        }
        if (actions.childElementCount) row.append(actions);
        rows.append(row);
      }
      messages.replaceChildren(rows);
      refresh.snapshot = snapshot;
    }
    status.textContent = pending ? '等待回复…' : '';
    if (pending) poll = setTimeout(refresh, 1000);
  } catch (error) {
    if (id !== refreshID) return;
    status.textContent = error.message;
    poll = setTimeout(refresh, 3000);
  }
}

function updateSend() {
  send.disabled = submitting || input.value.trim() === '';
  delegate.disabled = send.disabled || selectedTicket !== '';
}

input.addEventListener('input', updateSend);
document.getElementById('clear-ticket').addEventListener('click', () => selectTicket(''));
form.addEventListener('submit', async event => {
  event.preventDefault();
  const text = input.value.trim();
  const documentText = attachment.value;
  const isDelegation = event.submitter === delegate;
  if (submitting || !text) return;
  if (new TextEncoder().encode(text).length > inputLimit) {
    status.textContent = '消息太长，请缩短后发送。';
    return;
  }
  if (new TextEncoder().encode(documentText).length > inputLimit) {
    status.textContent = '附加文本太长，请缩短后发送。';
    return;
  }
  if (retry?.text !== text || retry?.document !== documentText ||
      retry?.ticketId !== selectedTicket || retry?.delegate !== isDelegation) {
    retry = {
      messageId: crypto.randomUUID(), text, document: documentText,
      ticketId: selectedTicket, delegate: isDelegation,
    };
  }
  submitting = true;
  updateSend();
  status.textContent = '正在发送…';
  try {
    await request({
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify(retry),
    });
    if (input.value.trim() === text && attachment.value === documentText) {
      input.value = '';
      attachment.value = '';
      document.getElementById('attachment').open = false;
    }
    retry = undefined;
    await refresh();
    refreshThreads().catch(error => { status.textContent = error.message; });
  } catch (error) {
    status.textContent = error.message;
  } finally {
    submitting = false;
    updateSend();
  }
});

document.getElementById('new-thread').href = '/agent-chat?thread=' + crypto.randomUUID();
refreshThreads().catch(error => { status.textContent = error.message; });
refresh();
