# TalosCon 2026 demo charts

Helm charts and images for a live demo at TalosCon 2026: two hospitals exchanging medical documents through a simplified IHE XDS/XCA network, deployed by Argo CD onto Talos clusters.

- `connector`: runs in a hospital, receives HL7 v2 messages from a simulated EMR and registers documents with its bridge.
- `bridge`: runs in the cloud, one per hospital plus a central bridge that connects them.
- `images/`: the Go source for both (`docker build --build-arg APP=connector|bridge images`).

**This is demo software.** It is simplified on purpose: synthetic patients, data kept in memory, no authentication, and only a sketch of the IHE transactions. It is not representative of production software and must not be used with real patient data.
