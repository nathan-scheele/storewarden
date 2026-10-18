# storewarden

storewarden lets software agents on a tailnet use one Shopify store through a fixed set of operations. The tailnet's policy file decides who may use each operation.

## Language

### Who and what

**Store**:
The one Shopify store that a storewarden instance acts on.
_Avoid_: shop, merchant, tenant

**Caller**:
The tailnet node making a request, as Tailscale identifies it, plus the user who owns the node unless the node is tagged.
_Avoid_: client, user, agent

### Operations

**Tool**:
A named, fixed operation on the Store that a Caller can invoke.
_Avoid_: endpoint, query, command

**Read tool**:
A Tool that never changes the Store.

**Write tool**:
A Tool that can change the Store.
_Avoid_: mutation (that's the Shopify API's word for the call underneath)

**Dry run**:
A Write tool call that reports the change it would make without making it. Every Write tool call is a dry run unless the Caller explicitly opts out.
_Avoid_: preview, simulation, plan

**Idempotency key**:
A Caller-chosen value attached to a real write. If the same write is retried with the same key, it takes effect only once.
_Avoid_: request ID, nonce

**Expected quantity**:
The quantity the Caller believes is current when it asks for a change. If the Store disagrees, the write is rejected.
_Avoid_: old value, compare value

### Permission

**Write mode**:
An instance-wide setting, off by default. When it is off, Write tools don't exist at all, whatever the Grants say.
_Avoid_: write flag, unsafe mode

**Grant**:
A rule in the tailnet policy file that lets matching Callers use specific named Tools.
_Avoid_: permission, role, scope

**Access scope**:
A Shopify permission held by storewarden's own credentials. Together they set the limit on what any Tool can reach.
_Avoid_: scope (alone), grant

### Record-keeping

**Audit record**:
The permanent account of one attempted Tool call: who made it, what it asked for, whether it was allowed, and what happened.
_Avoid_: log line, event
