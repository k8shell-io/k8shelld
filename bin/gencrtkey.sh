#!/bin/bash

# Set default values
CERT_NAME=${1:-server}
DAYS_VALID=365

echo "Generating private key..."
openssl genrsa -out ${CERT_NAME}.key 2048

echo "Generating certificate signing request..."
openssl req -new -key ${CERT_NAME}.key -out ${CERT_NAME}.csr \
  -subj "/C=US/ST=State/L=City/O=Organization/OU=Unit/CN=localhost"

echo "Generating self-signed certificate..."
openssl x509 -req -days ${DAYS_VALID} -in ${CERT_NAME}.csr -signkey ${CERT_NAME}.key -out ${CERT_NAME}.crt

echo "Certificate and key have been generated:"
echo " - Certificate: ${CERT_NAME}.crt"
echo " - Private Key: ${CERT_NAME}.key"

# Optional: Clean up the CSR
rm -f ${CERT_NAME}.csr
