curl -s -X PUT "http://localhost:8000/api/apps/digitorn-code/sessions/5c0431d3-4a2a-44b9-a958-4cb1c82e0e94/model" \
  -H "X-User-ID: 3442647483eb4a458e4feffcbe4b9429" \
  -H "Content-Type: application/json" \
  -d '{"agent":"main","model":"zen-mimo-v2.5-free"}'