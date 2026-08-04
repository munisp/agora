.PHONY: up up-voice down logs ps config smoke seed topics dbt trino eval ab-test loadtest-voice validate-packs install-pack clean

up:            ## Full platform (middleware + services + web)
	docker compose up -d --build

up-voice:      ## Platform + local voice models (ollama/piper)
	docker compose --profile voice up -d --build

down:          ## Stop everything
	docker compose down

clean:         ## Stop and delete volumes (DESTRUCTIVE)
	docker compose down -v

logs:          ## Tail all logs
	docker compose logs -f --tail=200

ps:
	docker compose ps

config:        ## Validate merged compose config
	docker compose config -q && echo "compose OK"

topics:        ## Re-run Kafka topic init
	docker compose run --rm kafka-topics

seed:          ## Seed demo tenant "acme" + catalog + availability
	./scripts/seed-demo.sh

validate-packs: ## Validate industry packs + registry checksums (Wave 5 #6)
	python3 scripts/validate_pack.py validate industries/*.yaml
	python3 scripts/validate_pack.py validate-index industries/index.json

install-pack:  ## Install a community pack: make install-pack PACK=<url-or-path> [VERSION=v AUTHOR=a]
	./scripts/install-pack.sh $(PACK) $(if $(VERSION),--version $(VERSION)) $(if $(AUTHOR),--author $(AUTHOR))

smoke:         ## End-to-end smoke test through APISIX gateway
	./scripts/smoke-test.sh

dbt:           ## Run lakehouse dbt marts (silver+gold)
	docker run --rm --network opendesk -v $$PWD/infra/lakehouse/dbt:/usr/app ghcr.io/dbt-labs/dbt-trino:1.8.x build

trino:         ## Open a trino CLI against the lakehouse
	docker exec -it trino trino --catalog iceberg --schema gold

eval:          ## Voice agent eval harness (scenarios -> /voice/chat + LLM judge)
	python3 services/voice-agent-runtime/eval/eval.py $(EVAL_ARGS)

ab-test:       ## A/B persona eval (needs EVAL_PERSONA_OVERRIDE=true runtime): make ab-test AB_ARGS="--tenant acme --persona-b services/voice-agent-runtime/eval/personas/salon_warm_concise.md [--promote]"
	python3 services/voice-agent-runtime/eval/ab_test.py $(AB_ARGS)

loadtest-voice: ## Voice load ramp 5->10->25->50 (needs live stack: make up-voice; see tests/load/README.md)
	python3 tests/load/voice_ramp.py $(LOADTEST_ARGS)

.PHONY: seed-all seed-ci seed-drift

seed-all:      ## Seed ALL platform CAC data (SPEC-W17 §8.5 9-step; needs postgres up): SEED_SCALE=1.0 make seed-all
	./scripts/seeds/bootstrap.sh

seed-ci:       ## CI seed at SEED_SCALE=0.05 with Kafka off (SEED_KAFKA=on make seed-ci to publish events)
	SEED_SCALE=0.05 SEED_KAFKA=off ./scripts/seeds/bootstrap.sh

seed-drift:    ## §8.7 #5 drift gate — fails if scripts/seeds/drift.sql returns any row (forwards SEED_SCALE)
	@out="$$(psql "$${SEED_DATABASE_URL:-postgres://opendesk:opendesk@localhost:5432/analytics_meta}" -At -v seed_scale="$${SEED_SCALE:-1.0}" -f scripts/seeds/drift.sql)"; \
	if [ -n "$$out" ]; then echo "$$out"; echo "seed-drift: DRIFT DETECTED (§8.7 #5)" >&2; exit 1; fi; \
	echo "seed-drift: OK (0 rows)"
