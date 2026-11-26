# Direct and group chat bridge

LoopForge offers a browser test entry and a local API for messaging adapters.
Both run the same conversation processing and A2A client. Each entry has its
own identity scope and stored conversation history.

## Browser entry

Set `AGENTFLOW_WEB_CHAT_USER_IDS` to a comma-separated list of LoopForge account
IDs. Set `AGENTFLOW_A2A_RPC_URL` and `AGENTFLOW_WEB_CHAT_A2A_TOKEN` on the same
process for a dedicated web principal. `AGENTFLOW_A2A_TOKEN` remains the SOP
credential and serves as the web fallback in a single-caller deployment.
Each allowed account sees a conversation link in the main navigation after sign-in
and can open `/agent-chat` directly. This route retains the
latest 20 visible turns in the selected conversation and submits the latest
four successful exchanges as bounded context. An empty account list disables
the browser entry.

The default conversation keeps the existing history. **新建对话** opens a new
thread. Its first submitted message makes it visible in the account's recent
conversation list. Each thread gets separate history. The browser includes
`?thread=<uuid>` in message and stop requests for that conversation. Ordinary
turns keep their order within the conversation. The dispatcher has two worker
slots, which may process different conversations or tickets.
Set `AGENT_WORKER_SLOTS=2` on the Runtime for the local
concurrency demo.

**委派任务** creates a durable ticket inside the current conversation.
**追问任务** selects that ticket for subsequent messages until the user exits
the selection. The ordinary composer uses its own history. A ticket receives
only its own prior turns, and its replies appear in the originating
conversation. A ticket can be followed from a recent message after a restart.

The message API accepts `delegate: true` to create a ticket or `ticketId` to
continue an existing one. Success returns the message ID and ticket ID.
Ticket creation is committed with the initial message. A repeated message
ID must keep the same ticket. A different account or conversation cannot use
that ticket ID. Turns are ordered within each ticket, and all tickets count
toward the existing account limits.

To enable automatic routing for normal sends, set
`AGENTFLOW_WEB_CHAT_ROUTER_A2A_TOKEN` to a separate Runtime credential with the
`router` profile. Explicit delegation and follow-up already have a target.
The browser receives its usual receipt before the background router runs.
LoopForge snapshots the latest two completed conversation turns and up to five
tickets in the originating conversation. The snapshot and Router Task ID
survive retries. To continue a ticket, the Router must quote a reference found
in the latest message and in exactly one candidate title. LoopForge checks
the reference and the ticket's current ownership before enqueueing execution.
Clarification is returned in the conversation without starting an Executor Run.
When the next message consists of a unique candidate title or title fragment,
the executor receives the original instruction and attachment with that ticket
selection. LoopForge rechecks the preceding clarification under the same
account and conversation, then freezes the combined input in the handoff. An
intervening message ends this one-turn continuation.

Two settings limit web work across all conversations of an account.
`AGENTFLOW_WEB_CHAT_MAX_PENDING_PER_USER` defaults to 8 unfinished messages.
`AGENTFLOW_WEB_CHAT_MAX_RUNNING_PER_USER` defaults to 2 started turns and
cannot exceed the pending limit. A started turn keeps its account slot while
reconnecting or waiting for cancellation. The local worker may use its free
slot for another account during that interval. API replicas use the same limits.

At the pending limit, a new message returns HTTP 429. An exact retry still
returns its original ID, and conflicting content or a different conversation
for that ID returns HTTP 409. Completion, failure and confirmed cancellation
free a pending slot. A queued, unstarted turn frees its slot when canceled.

For a single turn, the browser can attach up to 4,000 UTF-8 bytes of text.
LoopForge persists the attachment with its message and passes it to the Agent
Runtime as `attachment.txt`. A retry must keep the same attachment. The history
API returns an attachment indicator and the assistant's reply. The next turn
receives the bounded conversation history and any newly attached text.

The browser can stop an active turn with `POST /api/agent-chat/messages/{id}/cancel`.
The request requires the message owner's browser session and the same origin.
An unclaimed turn becomes `canceled` locally. For a claimed turn the app stores
the stop request, submits A2A `CancelTask` on its existing task, and reports
`stopping` until the remote task settles. If the task completed first, the UI
shows its reply. Repeating the stop request is safe. Canceled turns are omitted
from the inputs of later turns. This control is available in the browser.

LoopForge persists the bounded conversation history used to submit a web turn
and links the remote task as soon as its ID is known. A retry after a lost
submission response uses the same message ID, history and attachment. Once
linked, a replacement worker resumes with `GetTask`. Temporary transport
failures produce `reconnecting` or `stopping` and use a 1 to 32 second retry
delay. Other conversations may use the execution slot during that delay;
later turns of the same conversation stay queued.

The source Compose stack forwards the web and SOP variables to the platform
container. Set the Runtime URL to an address reachable from that container and
use the usual `./start.sh dev` command. The published release image predates this
entry.

### Tabbit Room as the web executor

Set `AGENTFLOW_WEB_CHAT_ROOM_A2A_RPC_URL` to the Room A2A gateway and provide a
dedicated `AGENTFLOW_WEB_CHAT_A2A_TOKEN`. LoopForge sends its authorized account
ID and a stable conversation scope. Tabbit owns execution history and Run
state. A branch in the existing web UI uses a distinct Room scope. This mode
currently supports text turns and cancellation. Existing attachments, Router
calls and SOP nodes need their corresponding executor support.

Finish or cancel in-flight web turns before changing the executor URL. They
keep their original A2A Task IDs after restarts. The gateway must have a
configured Tabbit account binding for each allowed LoopForge account.

## Messaging adapter entry

The optional LoopForge listener accepts normalized text events from a colocated
messaging adapter. LoopForge stores the message before acknowledging it, keeps
direct conversations per sender and shared conversations per group, and submits
each turn as a read-only A2A task. The adapter authenticates provider events,
maps sender and group IDs, and delivers the result to the originating chat.

## Configuration

Set the following variables on a LoopForge API or all-in-one process.

| Variable | Value |
| --- | --- |
| `AGENTFLOW_CHAT_BRIDGE_PORT` | Local port. The listener binds to `127.0.0.1`. Leave unset to disable. |
| `AGENTFLOW_CHAT_BRIDGE_ID` | Stable ID for this adapter and bot. |
| `AGENTFLOW_CHAT_BRIDGE_TOKEN` | Dedicated random bearer token of at least 32 bytes. |
| `AGENTFLOW_CHAT_BRIDGE_SENDERS` | Comma-separated list of allowed sender IDs. |
| `AGENTFLOW_CHAT_BRIDGE_GROUPS` | Comma-separated list of allowed group IDs. Leave unset for direct chat only. |
| `AGENTFLOW_A2A_RPC_URL`, `AGENTFLOW_A2A_TOKEN` | Read-only Agent Runtime's JSON-RPC URL and bearer token. |

Use the same bridge ID, A2A endpoint and allowlists across replicas. Keep the
transport token and the A2A token in separate deployment secrets. Route the
adapter to a model endpoint approved for the content it handles.

The source Compose overlay also forwards the message bridge variables. The
ingress port stays private. Run a colocated adapter in the platform service's
network namespace so it can reach that port over loopback.

## Adapter contract

Send `POST /internal/chat-bridge/messages` with a bearer token and JSON.

```json
{
  "messageId": "stable-provider-message-id",
  "kind": "group",
  "chatId": "allowed-group",
  "senderId": "allowed-sender",
  "mentioned": true,
  "text": "Summarize our previous answer."
}
```

Use `kind: "single"` and omit `chatId` and `mentioned` for direct messages.
For groups, set `mentioned` only after matching the provider's structured
mention list against the bot's own identity. Ignore bot-authored events.

The response is HTTP 202 with an `id`. Duplicate events return the same ID
when their normalized content matches. Poll
`GET /internal/chat-bridge/messages/{id}` with the same token. The `state`
is `queued`, `running`, `replied`, or `reply_failed`. The last two states
contain an available `reply`. Use the returned ID as the delivery key so an
adapter retry does not send a second message.

This contract handles text and up to four bounded prior exchanges. Group
history includes all accepted participants in that group. The provider-specific
callback server and delivery API are deployed separately from LoopForge.

Deploy the Runtime with support for the A2A `conversation` data part before
enabling these entry points. The first turn sends an empty history, and subsequent
turns send prior user and assistant messages. The browser attachment requires a
Runtime that accepts both `conversation` and `documents` in the same data part.
