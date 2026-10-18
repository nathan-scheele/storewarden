# One Store per process

A storewarden instance acts on exactly one Store. An operator with several Stores runs one instance, and one tailnet node, per Store. This keeps the tailnet's identity boundary and the Store boundary the same thing: a Grant to `tag:storewarden-a` can only ever reach Store A, each process holds one Store's client secret, and Grants and Audit records need no store field.

## Considered Options

- **Several Stores per process, with the Store as part of the Grant.** Rejected for v2. It saves the operator a node per extra Store, but it is the only candidate that makes the threat model worse: a typo in a Grant can reach the wrong Store, and one compromised process holds every Store's secret. It would also change the meaning of Store, the Grant format and the Audit record.

## Consequences

Revisit this only if running one node per Store becomes a real operational burden, and reopen the Grant format at the same time.
