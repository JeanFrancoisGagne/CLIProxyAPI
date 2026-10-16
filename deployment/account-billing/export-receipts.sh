#!/bin/sh
# The SSH key can invoke only this read-only exporter; JSON travels on stdin.
exec /usr/bin/docker exec -i --user node openclaw-openclaw-gateway-1 \
  /usr/bin/python3 /home/node/.openclaw/workspace/tools/account_billing/receipt_exporter.py \
  --mailbox jfgagne@nera.software
