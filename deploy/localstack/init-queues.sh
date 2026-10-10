#!/usr/bin/env bash
# Executado pelo LocalStack quando ele fica pronto (hook ready.d).
# Cria as filas FIFO do desafio. É idempotente: rodar de novo não duplica nada.
set -euo pipefail

MAIN_QUEUE="wager-transactions.fifo"
DLQ="wager-transactions-dlq.fifo"
EVENTS_QUEUE="wager-events.fifo"

# Decisões documentadas (devem constar também no ARCHITECTURE.md):
#  - maxReceiveCount = 5  -> após 5 recebimentos sem remoção, a mensagem vai para a DLQ
#  - VisibilityTimeout = 60s -> maior que o tempo esperado de processamento de uma operação
#  - ContentBasedDeduplication = false -> usamos MessageDeduplicationId explícito
MAX_RECEIVE_COUNT=5
VISIBILITY_TIMEOUT=60

echo "[init-queues] criando DLQ: ${DLQ}"
DLQ_URL=$(awslocal sqs create-queue \
  --queue-name "${DLQ}" \
  --attributes FifoQueue=true,ContentBasedDeduplication=false \
  --query 'QueueUrl' --output text)

DLQ_ARN=$(awslocal sqs get-queue-attributes \
  --queue-url "${DLQ_URL}" \
  --attribute-names QueueArn \
  --query 'Attributes.QueueArn' --output text)

echo "[init-queues] criando fila principal com redrive: ${MAIN_QUEUE}"
ATTRS_FILE=$(mktemp)
cat > "${ATTRS_FILE}" <<EOF
{
  "FifoQueue": "true",
  "ContentBasedDeduplication": "false",
  "VisibilityTimeout": "${VISIBILITY_TIMEOUT}",
  "RedrivePolicy": "{\"deadLetterTargetArn\":\"${DLQ_ARN}\",\"maxReceiveCount\":\"${MAX_RECEIVE_COUNT}\"}"
}
EOF
awslocal sqs create-queue \
  --queue-name "${MAIN_QUEUE}" \
  --attributes "file://${ATTRS_FILE}" >/dev/null
rm -f "${ATTRS_FILE}"

echo "[init-queues] criando fila de eventos de saída: ${EVENTS_QUEUE}"
awslocal sqs create-queue \
  --queue-name "${EVENTS_QUEUE}" \
  --attributes FifoQueue=true,ContentBasedDeduplication=false >/dev/null

echo "[init-queues] filas prontas:"
awslocal sqs list-queues
