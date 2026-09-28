# pcuintegrator_go

Jednokierunkowa synchronizacja zadań **ClickUp → Productive.io**, a w tym samym runie **fanout zadań ClickUp i Productive do DeliverIT RCP** (sekcja [Fanout do DeliverIT](#fanout-do-deliverit)). Port `pcuIntegrator_dotnet` na Go, uruchamiany jako **Cloudflare Container** za **Workerem**, odpalany **`GET /sync` z kluczem w headerze** — bez wewnętrznego timera. Między runami wszystko śpi.

```
klient  ──GET /sync  Authorization: Bearer <SYNC_KEY>──▶  Worker (src/index.ts)
                                                            │ auth stałoczasowa, 401/405/404
                                                            ▼
                                                     SyncContainer (Durable Object)
                                                            │ lease lock → 409 gdy run trwa
                                                            ▼
                                                     kontener: Go HTTP :8080
                                                            │
                          ┌─────────────────────────────────┴───────────────────────────┐
                          ▼ etap 1                                                       ▼ etap 2 (fanout)
              api.clickup.com  +  api.productive.io              http://deliverit.internal (bez klucza)
                                                                           │ outbound handler Workera:
                                                                           │ 2 trasy, Authorization z sekretu
                                                                           ▼
                                                         service binding DELIVERIT → Worker deliverit-internal
                                                                           ▼
                                                             DeliverIT /api/integrations/*
```

Cron Trigger `0 */4 * * *` wchodzi tą samą ścieżką (odpowiednik `PeriodicTimer(4h)` z .NET).

## Trigger

```bash
# normalny run
curl -H "Authorization: Bearer $SYNC_KEY" https://pcuintegrator.<subdomena>.workers.dev/sync

# dry run — w pełni read-only, pokazuje dokładne body, które by poleciały
curl -H "Authorization: Bearer $SYNC_KEY" 'https://.../sync?dry_run=true&explain=true' | jq

# świadomy większy import (podniesienie bezpiecznika na jeden request)
curl -H "Authorization: Bearer $SYNC_KEY" 'https://.../sync?max_creates=400'

# stan ostatniego / trwającego runu
curl -H "Authorization: Bearer $SYNC_KEY" https://.../status | jq
```

Klucz **wyłącznie w headerze**. Nigdy w query stringu — dzięki temu unfurlery Slacka/Teams, skanery maili i historia przeglądarki dostają 401, a nie odpalają synchronizacji produkcyjnej. `HEAD`, `POST` i wszystko poza `GET /sync` i `GET /status` to 405/404.

### Endpointy

| | |
|---|---|
| `GET /sync` | cały sync, synchronicznie; JSON z podsumowaniem |
| `GET /status` | stan kontenera + lease + ostatni run |
| `?dry_run=true` | zero zapisów, wymuszone na najniższej warstwie klienta HTTP |
| `?explain=true` | dołącz plan i wyrenderowane body również przy prawdziwym runie |
| `?max_creates=N` / `?max_writes=N` | jednorazowe podniesienie bezpiecznika |

Kody: `200` OK · `207` część zapisów padła (także: błąd etapu DeliverIT — `deliverit.errors`) · `422` etap ClickUp → Productive przerwany (`aborted`; etap DeliverIT **nigdy** nie daje 422) · `409` inny run trwa · `401`/`405`/`404` · `503` kontener niedostępny.

## Sekrety

```bash
npx wrangler secret put SYNC_KEY            # openssl rand -hex 32
npx wrangler secret put CLICKUP_TOKEN       # surowy pk_… , bez "Bearer"
npx wrangler secret put PRODUCTIVE_TOKEN    # X-Auth-Token
npx wrangler secret put PRODUCTIVE_ORG_ID   # X-Organization-Id
# opcjonalnie na czas rotacji:
npx wrangler secret put SYNC_KEY_PREVIOUS
# opcjonalnie — etap DeliverIT (brak = etap wyłączony z ostrzeżeniem):
npx wrangler secret put DELIVERIT_API_KEY   # dit_… z DeliverIT → /integracje (pokazany raz)
```

`SYNC_KEY` jest sprawdzany **tylko w Workerze** i nigdy nie trafia do kontenera — inaczej wylądowałby w logach kontenera i w `/proc/self/environ`. Z tego samego powodu **`DELIVERIT_API_KEY` też nie trafia do kontenera**: używa go wyłącznie outbound handler Workera, a kontener dostaje tylko jego prefiks (`DELIVERIT_API_KEY_PREFIX`, `dit_` + 8 znaków). Nigdy nie używaj `image_vars`: to build args, wypalają się w warstwach obrazu.

Uwaga na sprzężenie przy rotacji: `wrangler secret put` tworzy nową wersję Workera, a jej deploy wywołuje rollout kontenera → SIGTERM działającego runu. Rotuj, gdy nic nie leci.

## Konfiguracja

Wszystkie id i etykiety są **wkompilowane w binarkę** (dokładnie te, które .NET miał zaszyte w `PCUIntegratorService.cs` i `MappingExtensions.cs`) i nadpisywalne env. W `wrangler.jsonc` → `vars` siedzą tylko te, po które faktycznie się sięga; reszta jest w `.dev.vars.example`.

| Zmienna | Default | Uwagi |
|---|---|---|
| `CLICKUP_LIST_ID` | `900501332334` | |
| `PRODUCTIVE_PROJECT_ID` | `860646` | |
| `PRODUCTIVE_TASK_LIST_ID` | `2385788` | |
| `PRODUCTIVE_CF_CLICKUP_ID` | `242457` | custom field = klucz łączący |
| `PRODUCTIVE_CF_CLICKUP_TAGS` | `242565` | tagi, `", "` |
| `PRODUCTIVE_STATUS_OPEN_ID` / `_DONE_ID` | `161082` / `161083` | |
| `PRODUCTIVE_DONE_STATUS_NAMES` | `Closed` | mapowanie po **nazwie**, jak w .NET |
| `CLICKUP_DONE_STATUSES` | `zawieszone,gotowe do wydania,wydane` | dopasowanie dokładne |
| `CLICKUP_ESTIMATE_FIELD_IDS` | dwa uuid „Szybka wycena" | API + FRONT |
| `CLICKUP_ESTIMATE_LABEL_MINUTES` | JSON, 5 etykiet | luki `12h-16h`/`32h-40h` zostają |
| `CLICKUP_INCLUDE_CLOSED` | `false` | **nie włączaj przypadkiem** — patrz niżej |
| `PRODUCTIVE_TITLE_MAX` | `140` | limit Productive; dłuższe tytuły są obcinane |
| `PRODUCTIVE_MAX_SUBTASK_DEPTH` | `1` | głębsze zagnieżdżenia lądują jako płaskie zadania |
| `MERGE_CUSTOM_FIELDS` | `true` | `false` = destrukcyjne zachowanie .NET |
| `ESTIMATE_CLEAR_MODE` | `ignore` | `null` = kasuje estymaty wpisane ręcznie |
| `ALLOW_REPARENT` | `false` | |
| `PRODUCTIVE_RPS` | `1.0` | sufit walidowany na 2.2 (4000/30min) |
| `PRODUCTIVE_BURST_RPS` | `8.0` | sufit 10 (100/10s) |
| `CLICKUP_RPS` | `1.5` | ~100/min per token |
| `MAX_CREATES` / `MAX_WRITES` | `25` / `300` | bezpieczniki |
| `SYNC_TIMEOUT` | `10m` | walidowane ≤ 12m; obejmuje oba etapy |
| `MAX_RETRIES` | `4` | także DeliverIT (5 prób) |
| `LOG_LEVEL` | `info` | JSON na stdout |
| `DELIVERIT_BASE_URL` | `""` (wył.) | w `wrangler.jsonc`: `http://deliverit.internal`; walidowane — patrz [Fanout do DeliverIT](#fanout-do-deliverit) |
| `DELIVERIT_RPS` | `2` | walidowane w (0, 4] = ≥ 250 ms między żądaniami |
| `DELIVERIT_API_KEY` | — | **sekret Workera**, do kontenera nie trafia; brak ≠ błąd startu |

Brakujący `CLICKUP_TOKEN` / `PRODUCTIVE_TOKEN` / `PRODUCTIVE_ORG_ID` → proces **nie startuje**. Brakujący albo zły klucz DeliverIT — **startuje** (etap DeliverIT wyłączony, ostrzeżenie/błąd w sekcji `deliverit`); błędny `DELIVERIT_BASE_URL` / `DELIVERIT_RPS` — nie startuje, jak każda walidowana zmienna. Puste `PRODUCTIVE_TOKEN` prowadziłoby do 401 na każdym czytaniu, a to najgroźniejszy stan w całym programie (patrz „Dlaczego czytanie przerywa run").

## Jak czytać odpowiedź

```json
{
  "run_id": "20260728T085008Z-7a4e5a1d",
  "duration_ms": 41230,
  "clickup":    { "tasks": 412, "pages": 5, "subtasks": 37, "status_types": { "wydane|closed": 61 } },
  "productive": { "tasks": 388, "total_count": 388, "without_clickup_id": 4,
                  "workflow_status_names": { "161082": "Open", "161083": "Closed" } },
  "planned": { "create": 5, "update": 11, "skip": 396, "conflict": 0 },
  "created": 5, "updated": 11, "skipped": 396, "failed": 0, "ambiguous": 0,
  "reasons": { "new": 5, "title": 9, "tags": 2 },
  "warnings": [], "errors": [],
  "deliverit": { "enabled": true, "projects": 3, "created": 12, "renamed": 1, "unchanged": 187, "skipped": 3, "…": "…" }
}
```

`warnings`/`errors`/`aborted` na górze dotyczą etapu ClickUp → Productive; etap DeliverIT ma własne w sekcji `deliverit` (pełny przykład: [Fanout do DeliverIT](#przykładowa-odpowiedź-sekcja-deliverit)).

**`reasons` to najważniejsze pole.** Zbiegnięty system robi 0 zapisów; `{"tags": 412}` w każdym runie znaczy, że coś nigdy nie konwerguje i każdy run przepisuje pół listy. `status_types` i `workflow_status_names` są tam po to, żeby dwie otwarte kwestie z sekcji „Do zmierzenia" rozstrzygnąć danymi, bez osobnego curla.

`ambiguous` to zapisy, których wynik jest nieznany (utracona odpowiedź POST-a). Nie są ponawiane — patrz niżej.

## Bezpieczniki i decyzje, które warto znać

**Dlaczego czytanie przerywa run.** Każdy non-2xx przy czytaniu Productive kończy run bez zapisu. .NET jest bezpieczny **przez przypadek**: `GetFromJsonAsync` rzuca na non-2xx i wyjątek leci do `catch`. Naiwny port (`if err != nil` + `Decode`) zdekodowałby stronę błędu jako pustą listę, uznał, że Productive jest puste, i **wysłał POST każdego zadania z ClickUp** — każde ze wpisanym `242457`, więc nie do odróżnienia od prawdziwych. Dodatkowo liczba zebranych zadań jest porównywana z `meta.total_count`: krótszy odczyt (task przesunął się między stronami w trakcie czytania) też przerywa run.

**Bezpieczniki `MAX_CREATES` / `MAX_WRITES`.** Przekroczenie = abort **przed** pierwszym zapisem, z planem w odpowiedzi. To software bez nadzoru za triggerem co 4h; bez tego regresja w mapowaniu przepisuje całą listę, zanim ktokolwiek zauważy.

**Retry tylko tam, gdzie bezpieczny.** GET: 429/5xx/transport. PATCH: 429/5xx (body w pełni opisuje stan docelowy). **POST: wyłącznie 429** — 429 dowodzi, że zapis się nie stał, natomiast timeout/5xx/reset są **niejednoznaczne** i retry stworzyłby bliźniaka. Utrata odpowiedzi POST-a jest samonaprawialna: klucz `242457` powstaje atomowo z taskiem, więc następny run go znajdzie i zrobi PATCH. **Nie „naprawiaj" tego dodając retry.**

**Duplikaty `242457`.** Jeśli dwa taski Productive zgłaszają ten sam ClickUp id, **nie zapisujemy do żadnego** i raportujemy oba id. .NET w tej sytuacji wywala się na `ToDictionary` przy każdym ticku (trwała awaria), a naiwne `map[k]=v` cicho wybrałoby jednego bliźniaka.

**Lease lock w Durable Object.** Mutex w procesie Go nie chroni przed dwiema generacjami kontenera: przy rollout/recyklingu hosta stary proces ma 15 minut na dokończenie, a DO rutuje nowe requesty do następcy — dwa runy czytałyby Productive przed pierwszym zapisem i oba wysłałyby POST tego samego taska. DO to jedyne miejsce ze stanem przeżywającym restart kontenera. Mutex w Go zostaje jako druga linia (i dla lokalnego `docker run`).

**Cykl życia kontenera.** `sleepAfter = "20m"` to wyłącznie backstop — deadline runu to 10 min, więc timer nie może wystrzelić w trakcie. Po czystym zakończeniu DO woła `stop()`, więc kontener ginie od razu po runie. Po zerwanym połączeniu `stop()` **nie** jest wołane, a lease wygasa sam: proces Go pracuje na kontekście odpiętym od requestu, więc zerwane połączenie kosztuje podsumowanie, nie run — wynik zostaje w `/status` i w logach.

## Fanout do DeliverIT

Etap 2 każdego runu: zadania z list ClickUp i Productive powiązanych z projektami w **DeliverIT RCP** trafiają tam jako zadania projektu (do ewidencji czasu). Klient DeliverIT i jego testy przeniesiono z niewdrożonego prototypu `~/GolandProjects/syncBridge`. Kod: `container_src/deliverit.go` (kontrakt, klient), `container_src/fanout.go` (etap), `src/index.ts` (outbound handler).

### Co robi i kiedy

W **każdym** runie (cron `0 */4 * * *` UTC i ręczne `GET /sync`), **po** etapie ClickUp → Productive i **niezależnie od jego wyniku** — także gdy odczyt Productive przerwał run (`aborted: "fetch_productive"`), bo fanout potrzebuje tylko zadań źródła. Etap DeliverIT nigdy nie przerywa etapu Productive.

1. `GET /api/integrations/projects` — DeliverIT zwraca projekty powiązane z listą w źródle (`taskList.source`: `1` = ClickUp, `2` = Productive). Mapę projekt ↔ lista ustawia Pracodawca w DeliverIT (`/projekty/:id` → „Źródło zadań”); pcuintegrator nie ma własnej.
2. Dla każdego projektu (sekwencyjnie) zadania listy:
   - **ClickUp, lista = `CLICKUP_LIST_ID`** → zadania **już pobrane w etapie 1**, bez drugiego odczytu. Jeśli ten odczyt w tym runie padł — projekt `failed` („not re-read”), bez ponawiania. Gdy etap 1 nie doszedł do ClickUp (padł odczyt Productive), fanout czyta listę sam.
   - **ClickUp, inna lista** → istniejący klient ClickUp z **tymi samymi filtrami** co etap 1: `subtasks=true`, `include_closed` wg `CLICKUP_INCLUDE_CLOSED`. Ta sama lista daje więc ten sam zbiór, niezależnie od drogi odczytu.
   - **Productive** → istniejący klient Productive (`tasks?filter[task_list_id]=…`, bez `include`), **ten sam limiter** co etap 1 (limity Productive są na organizację) i ta sama asercja `meta.total_count`.
   - **Nieznane źródło** (nowa wartość `TaskSource` w DeliverIT) albo **brak tokenu źródła** → projekt `skipped` z ostrzeżeniem.
3. Mapowanie `{externalId: id zadania, name: tytuł}`. Nazwa z ClickUp to **pełny** tytuł (Productive dostaje obcięty do 140 znaków; DeliverIT sam tnie do 300 z „…”). Przed wysłaniem, każde odrzucenie z ostrzeżeniem: zadania łamiące reguły walidatora DeliverIT (`externalId` spoza `\A[A-Za-z0-9_-]{1,100}\z`, pusta nazwa — jedno takie zadanie dałoby `400` na **całą** paczkę), nazwy przycięte do 1000 znaków (run), duplikaty `externalId` → zostaje ostatni odczyt. Znaki NUL (U+0000) są usuwane z nazw bez ostrzeżenia (PostgreSQL nie przechowuje ich w `text`, a DeliverIT ich nie usuwa — jedna taka nazwa dałaby `500` na **całą** paczkę); nazwa pusta po usunięciu → `empty_name` z ostrzeżeniem. Paczki po **200** (serwer przyjmuje ≤ 500). Pusta lista = **jedna pusta paczka** (DeliverIT oznacza projekt jako zsynchronizowany).
4. `POST /api/integrations/projects/{id}/tasks/sync` — po stronie DeliverIT idempotentny upsert: zakłada nowe, zmienia nazwy, **niczego nie usuwa** (zadania mają wpisy czasu). Każda paczka niesie `taskList` dokładnie taki, jaki zwrócił `GET`.

Różnice względem syncBridge (świadome, bo lista ma dawać ten sam zbiór co w etapie 1): **podzadania są wysyłane** (ich generyczne nazwy typu „Testy” mogą kolidować → `skipped: name_taken`, ostrzeżenie w każdym runie) i **zamknięte zadania nie** (`include_closed=false`: zadanie zamknięte, zanim trafiło do DeliverIT, tam nie trafi).

### Droga sieciowa: service binding, klucz w Workerze

Kontener woła **`http://deliverit.internal`** — wirtualny host, bez klucza. Żądanie przechwytuje `deliverITOutbound` w `src/index.ts` (`SyncContainer.outboundByHost`, działa w runtime Workers poza sandboxem kontenera, na tej samej maszynie — stąd zwykłe HTTP bez `interceptHttps`) i:

- przepuszcza **wyłącznie** `GET /api/integrations/projects` i `POST /api/integrations/projects/{guid}/tasks/sync` (reszta → `403 OUTBOUND_ROUTE_DENIED`),
- buduje **nowe** żądanie na `https://internal.deliverit.pl` + ścieżka (schemat i host to kontrakt Workera DeliverIT: po `http` odpowiada 400/308, a .NET sprawdza `AllowedHosts`),
- przepisuje tylko `Content-Type`, `Accept`, `User-Agent` i **ustawia** `Authorization: Bearer` z sekretu `DELIVERIT_API_KEY` (bez `CF-*`, `X-Forwarded-*`, `Origin`, `Sec-Fetch-*`),
- woła `env.DELIVERIT.fetch()` (service binding do Workera `deliverit-internal`) i oddaje odpowiedź **bez zmian** (429/503 z `Retry-After`, 413 z pustym ciałem). Awaria bindingu → `502 DELIVERIT_BINDING_FAILED` (ponawiane).

Klucz **nie trafia do kontenera** (ani do jego env, ani do żądań kontenera — więc nie ląduje w Workers Logs jako nagłówek wywołania handlera). Kontener dostaje `DELIVERIT_API_KEY_PREFIX` (`dit_` + 8 znaków, ten sam prefiks co w `/integracje` i w logu .NET `user=api-key:dit_…`): brak = sekret nieustawiony → etap wyłączony z ostrzeżeniem; `invalid` = sekret w złym formacie → błąd. Ruch do ClickUp i Productive idzie jak dotąd wprost (przechwytywany jest tylko `deliverit.internal`).

### Konfiguracja

| Zmienna | Gdzie | Default | Uwagi |
|---|---|---|---|
| `DELIVERIT_BASE_URL` | `vars` | `""` = etap wył. | produkcja: `http://deliverit.internal` (tryb `proxied`). `https://…` = tryb `direct` (awaryjny/lokalny, niżej). `http` wyłącznie dla `deliverit.internal` i loopback; bez danych logowania, query i fragmentu. Błąd → proces nie startuje. |
| `DELIVERIT_RPS` | `vars` | `2` | (0, 4] → ≥ 250 ms między **każdą** próbą (także ponowieniem); limiter DeliverIT: 300/min per IP, przed uwierzytelnieniem. |
| `DELIVERIT_API_KEY` | sekret Workera | — | `dit_…` z DeliverIT → `/integracje` → „Wygeneruj klucz”. Otwiera wyłącznie `/api/integrations/*`. Brak → etap wyłączony z ostrzeżeniem (`200`), proces startuje. |
| `DELIVERIT_API_KEY_PREFIX` | liczy Worker | — | nie ustawiaj ręcznie; `containerEnvVars()` w `src/index.ts`. |

`MAX_RETRIES` (4 → 5 prób) i `SYNC_TIMEOUT` obowiązują też etap DeliverIT.

### Ponowienia, kody, statusy

- `429` (limiter DeliverIT) i `502`/`503`/`504` (Worker DeliverIT przy zimnym starcie kontenera, do ~45–65 s) → ponowienie po `Retry-After` (sekundy albo data HTTP, przycięte do 1–30 s; brak nagłówka = 10 s), do `MAX_RETRIES`. Timeout żądania 90 s (Worker DeliverIT czeka na zimny kontener do ~65 s).
- Błąd transportu → ponowienie **także dla POST**: w odróżnieniu od POST-a do Productive jest to bezpieczne, bo paczka to idempotentny upsert po `externalId`.
- `413` (pusta treść, bez problem+json), pozostałe `4xx` i `500` → **bez ponowień**. `500` to u DeliverIT m.in. wyścig na unikacie nazwy — następny run się goi.
- `3xx` → błąd `unexpected redirect …` **bez ponowień**; klient DeliverIT (osobny `http.Client`, nie ten od ClickUp/Productive) **nigdy nie podąża za przekierowaniem** — `net/http` przeniósłby `Authorization` na ten sam host (albo subdomenę) na dowolnym porcie i schemacie, także `http`, a `307`/`308` powtórzyłby paczkę. Cel z `Location` jest w błędzie bez query i fragmentu.
- `401 UNAUTHENTICATED` / `API_KEY_INVALID` → **koniec etapu** (dalsze projekty `skipped: stage_stopped`), błąd z podpowiedzią rotacji.
- `404 PROJECT_NOT_FOUND`, `409 PROJECT_TASK_LIST_MISMATCH` (powiązanie zmieniło się między GET a POST), `400 VALIDATION_FAILED` → projekt `failed`, pozostałe paczki tego projektu pominięte, kolejne projekty lecą dalej. Paczki przyjęte wcześniej zostają — następny run domyka resztę.
- Błędy mają postać `deliverit: 409 PROJECT_TASK_LIST_MISMATCH: <detail> [traceId …] (pole: komunikat)`; `traceId` znajdziesz w logu .NET DeliverIT. Odpowiedź bez problem+json → `deliverit: 403 without problem+json (text/html): <≤ 200 znaków>`, a przy `cf-mitigated: challenge` z dopiskiem o Bot Fight Mode.
- Błąd wewnętrzny (panika) w etapie DeliverIT → błąd `DeliverIT fanout aborted by an internal error …` w `deliverit.errors` (bieżący projekt `failed: internal_error`, stos w logu); proces, podsumowanie etapu 1 i zwolnienie lease zostają (`TestFanoutPanicBecomesAnErrorOfTheStage`).
- **Status `GET /sync`:** błąd etapu DeliverIT (`deliverit.errors` niepuste) → **`207`**, nigdy `422`. Ostrzeżenia (brak klucza, nieznane źródło, brak czasu, zadania pominięte) → status bez zmian. `422` zostaje wyłącznie dla przerwanego etapu ClickUp → Productive.
- **Klucz nigdy nie trafia do logu ani wyniku**: tylko `dit_` + 8 (`api_key_prefix`); w trybie `direct` klucz odbity przez pośrednika w treści odpowiedzi jest maskowany (testy `TestDeliverITKeyNeverAppearsInErrors`, `TestFanoutNeverLeaksTheKey`).
- **Błędy konfiguracji i klienta nie cytują wartości zmiennych `DELIVERIT_*`**, tylko je nazywają — klucz wklejony do złej zmiennej nie trafi do komunikatu błędu (a `DELIVERIT_BASE_URL` w postaci gołego klucza, z `http://` do obcego hosta, z danymi logowania, query albo fragmentem nie przejdzie walidacji startu). Poprawnie sparsowany URL bazowy widać natomiast celowo w logach informacyjnych `listening` i `deliverit fanout starting`. Błędy klienta podają trasę (`GET /api/integrations/projects`), nie URL; host i ścieżka z `DELIVERIT_BASE_URL` w błędzie transportu, w treści odpowiedzi i w `Location` są zastępowane przez `[DELIVERIT_BASE_URL host]` / `[DELIVERIT_BASE_URL path]` (w trybie `proxied` stały host `deliverit.internal` zostaje czytelny). Testy `TestDeliverITConfigErrorsNeverQuoteTheValue`, `TestDeliverITClientErrorsNeverQuoteTheBaseURL`.

### dry_run i explain

- `?dry_run=true` — etap **tylko liczy**: czyta listę projektów i zadania źródeł, filtruje, dzieli na paczki i raportuje `tasks`/`batches` oraz status projektu `planned`; **żadnego POST** (wymuszone też w najniższej warstwie klienta — `errReadOnly`). Dodatkowo (tylko w dry-runie, jeden GET na projekt) czyta **nazwę listy w źródle** → `project_results[].list_name` obok nazwy projektu DeliverIT; nieczytelna → `"?"` + ostrzeżenie. Nie sprawdza walidacji ani zapisu po stronie DeliverIT (pominięć `name_taken` itp.) — to widać dopiero w prawdziwym runie.
- `?explain=true` — każdy projekt dostaje `plan`: dokładne ciała paczek (`{taskList, tasks}`), tak jak by poleciały. Klucz jest nagłówkiem, więc w planie go nie ma. **Przed pierwszym prawdziwym runem sprawdź w `?dry_run=true&explain=true` pary `name` ↔ `list_name` i zadania w planie** — błędnie powiązanej listy nie cofnie się w aplikacji (procedura naprawy: runbook DeliverIT niżej).

```bash
curl -s -H "Authorization: Bearer $SYNC_KEY" 'https://pcuintegrator.<subdomena>.workers.dev/sync?dry_run=true&explain=true' | jq '.deliverit'
```

### Czas

Etap działa w tym samym `SYNC_TIMEOUT` co etap 1 (a lease w DO i 15-minutowy limit crona się nie zmieniają). Start etapu wymaga ≥ **90 s** zapasu (jeden pełny timeout żądania, obejmuje zimny start DeliverIT), każdy kolejny projekt ≥ 15 s; za mało → etap/pozostałe projekty **pominięte z ostrzeżeniem** („the next run sends everything/continues”). Ponowienie, którego `Retry-After` nie mieści się w pozostałym czasie, kończy się od razu błędem zamiast spać do deadline'u. SIGTERM → etap zatrzymuje się na granicy projektu/paczki.

### Przykładowa odpowiedź (sekcja `deliverit`)

```json
"deliverit": {
  "enabled": true,
  "mode": "proxied",
  "api_key_prefix": "dit_Ab3dE5gH…",
  "projects": 3, "synced_projects": 1, "skipped_projects": 1, "failed_projects": 1,
  "tasks": 214, "batches": 3,
  "created": 12, "renamed": 1, "unchanged": 187, "skipped": 3,
  "skipped_reasons": { "name_taken": 2, "invalid_external_id": 1 },
  "project_results": [
    { "id": "01997a3c-5e1f-7c2a-9b3d-000000000001", "name": "Portal klienta", "client_name": "ACME",
      "source": "clickup", "list_id": "900501332334", "status": "synced",
      "tasks": 202, "batches": 2, "created": 12, "renamed": 1, "unchanged": 187, "skipped": 3 },
    { "id": "01997a3c-5e1f-7c2a-9b3d-000000000002", "name": "Wdrożenie", "client_name": "Gamma",
      "source": "productive", "list_id": "4815162", "status": "failed", "reason": "batch_rejected",
      "tasks": 12, "batches": 1, "created": 0, "renamed": 0, "unchanged": 0, "skipped": 0 },
    { "id": "01997a3c-5e1f-7c2a-9b3d-000000000004", "name": "Nowe źródło", "client_name": "Delta",
      "source": "unknown(3)", "list_id": "77", "status": "skipped", "reason": "unknown_source",
      "tasks": 0, "batches": 0, "created": 0, "renamed": 0, "unchanged": 0, "skipped": 0 }
  ],
  "warnings": [
    "project \"Portal klienta\" (01997a3c-…-000000000001, clickup list 900501332334): task \"bad id\" skipped: id breaks DeliverIT's rule (1-100 of A-Z a-z 0-9 - _)",
    "project \"Portal klienta\" (…): DeliverIT skipped task 86c0abc13 \"Testy\": name_taken",
    "project \"Nowe źródło\" (…, unknown(3) list 77) skipped: task source unknown(3) is not supported by this pcuintegrator build (update it)"
  ],
  "errors": [
    "project \"Wdrożenie\" (…, productive list 4815162): batch 1 of 1: deliverit: 409 PROJECT_TASK_LIST_MISMATCH: Projekt nie jest powiązany z tą listą zadań. Pobierz listę powiązanych projektów ponownie. [traceId 00-abc-def-01]; remaining batches of this project skipped"
  ],
  "api": { "requests": 4, "retries": 0, "rate_limited": 0 }
}
```

Taki run zwraca `207`. **`skipped_reasons`**: `name_taken` / `external_id_in_other_project` — pominięte przez DeliverIT (napraw nazwę w źródle albo powiązanie), `invalid_external_id` / `empty_name` — odfiltrowane tutaj. Wyłączony etap: `"enabled": false` + `disabled_reason` (i ostrzeżenie, gdy brakuje klucza).

### Rotacja klucza

1. DeliverIT → `/integracje` → „Wygeneruj klucz” (nazwa np. „pcuintegrator”), skopiuj od razu (pokazany raz).
2. **Gdy nic nie leci** (`/status` → `lease_active: false`; cron co 4 h o pełnych godzinach UTC): `npx wrangler secret put DELIVERIT_API_KEY`. Uwaga: to **nowa wersja Workera → rollout kontenera → SIGTERM trwającego runu** (to samo sprzężenie co przy `SYNC_KEY`). Opcja bez rolloutu na przyszłość: Secrets Store.
3. `curl … '/sync?dry_run=true'` → `deliverit.enabled: true`, `api_key_prefix` = prefiks nowego klucza, `deliverit.errors` puste, `projects` > 0.
4. Unieważnij stary klucz w `/integracje` (działa natychmiast). Przy podejrzeniu wycieku — najpierw unieważnij, potem krok 1.

### Pierwsze wdrożenie (kroki człowieka)

Kolejność ma znaczenie: **Worker `deliverit-internal` (z trasami `/api/integrations/*`) musi być wdrożony przed pcuintegrator** — service binding wskazuje go z nazwy, a deploy z bindingiem do nieistniejącego Workera się nie uda (i zablokowałby też wdrożenia etapu Productive).

```bash
# 1. DeliverIT: /integracje → „Wygeneruj klucz”; /projekty/:id → „Źródło zadań” dla projektów
# 2. pcuintegrator:
npx wrangler secret put DELIVERIT_API_KEY          # gdy nic nie leci
npx wrangler deploy                                  # albo: make deploy (najpierw testy)
# odczekaj kilka minut (provisioning kontenera), potem:
curl -s -H "Authorization: Bearer $SYNC_KEY" 'https://pcuintegrator.<subdomena>.workers.dev/sync?dry_run=true&explain=true' | jq '.deliverit'
curl -s -H "Authorization: Bearer $SYNC_KEY" 'https://pcuintegrator.<subdomena>.workers.dev/sync' | jq '.deliverit | {projects, synced_projects, failed_projects, created, renamed, unchanged, skipped, errors}'
```

**Po pierwszym prawdziwym przebiegu sprawdź:**

- Cloudflare → strefa `deliverit.pl` → Security → Events: **brak** zdarzeń dla `/api/integrations` (service binding nie przechodzi przez strefę — jeśli są, ruch idzie publicznie i coś jest źle skonfigurowane).
- Log .NET DeliverIT (Workers & Pages → `deliverit-internal` → Containers → Logs): `user=api-key:dit_…` z prefiksem nowego klucza; zanotuj **adres IP** przy tych żądaniach (patrz „Znane ryzyka”).
- DeliverIT `/projekty/:id` → „ostatnia synchronizacja” ustawiona (`TaskListSyncedAt`).

Runbook po stronie DeliverIT (klucze, powiązania list, „Błędnie powiązana lista zadań”, dezaktywacja Pracodawcy nie unieważnia kluczy, klucze po odtworzeniu kopii): `deliverit-internal/docs/WDROZENIE.md` (lokalnie `/Users/gojanek/deliverit-internal/docs/WDROZENIE.md`) → „Integracja zadań”.

### Dlaczego nie publiczny internet — wnioski z researchu (2026-09-23)

- **Egress wprost z kontenera** (`https://internal.deliverit.pl` z kontenera) przechodzi przez pełny stos bezpieczeństwa strefy `deliverit.pl`. **Bot Fight Mode** (plan Free) działa poza Ruleset Engine: **reguła WAF Skip go nie omija** („you cannot skip Bot Fight Mode, only Super Bot Fight Mode”), pomija go tylko reguła **IP Access „Allow”** — a Containers **nie mają stałego ani udokumentowanego IP egress**. Na Free zostaje wyłączenie Bot Fight Mode dla całej strefy. Na Pro+ (Super Bot Fight Mode) wystarczy reguła custom Skip `(http.host eq "internal.deliverit.pl" and starts_with(http.request.uri.path, "/api/integrations/"))` z pominięciem: All Super Bot Fight Mode rules, rate limiting rules, Browser Integrity Check, Security Level.
- **Browser Integrity Check** jest domyślnie włączony i wyzywa klientów bez `User-Agent` / z niestandardowym — klient wysyła jawny `User-Agent: pcuintegrator/1 (+deliverit-internal)`.
- Limiter DeliverIT (300/min per IP, IPv6 po /64, **przed** uwierzytelnieniem) widziałby przy egressie z kontenera nieznany, potencjalnie zmienny adres.
- **Nigdy `fetch("https://internal.deliverit.pl")` z Workera pcuintegrator**: subrequest między strefami dostaje bezwarunkowo `CF-Connecting-IP = 2a06:98c0:3600::103`, wspólny dla Workerów **wszystkich** klientów Cloudflare → jedno wiadro limitera z obcymi (a „Allow” dla tego IP otworzyłoby strefę dla wszystkich Workerów świata).
- **Service binding** („not reachable via the public Internet”, tylko w obrębie konta) omija strefę w całości: bez Bot Fight Mode, WAF i BIC na ścieżce — **nie** dodawaj reguły IP Access ani Skip i nie wyłączaj Bot Fight Mode dla pcuintegrator. Krok „IP Access Allow dla IP maszyny” z runbooka DeliverIT (wariant syncBridge) nie dotyczy pcuintegrator.

### Tryb `direct` — tylko lokalnie albo awaryjnie

`DELIVERIT_BASE_URL=https://…` + `DELIVERIT_API_KEY` **w env kontenera**. Tak działa `make run` (docker bez Workera: `.dev.vars` z `DELIVERIT_BASE_URL=https://internal.deliverit.pl` i kluczem — to **produkcja** DeliverIT). W produkcji wyłącznie awaryjnie, gdyby service binding był niemożliwy — wymaga to zmiany kodu (`"DELIVERIT_API_KEY"` w `CONTAINER_ENV_KEYS` w `src/index.ts`; Worker celowo go dziś nie przekazuje), zmiany `vars` oraz po stronie strefy: wyłączenia Bot Fight Mode (Free) albo reguły Skip (Pro+) z punktu wyżej, i zgody na nieudokumentowany egress IP w limiterze.

### Znane ryzyka

- Dokumentacja nie mówi wprost, że wywołanie przez service binding omija funkcje strefy (wynika to z „without going over the Internet”) ani czy platforma dokłada wtedy `CF-Connecting-IP`. Worker DeliverIT przepisuje `CF-Connecting-IP` do `X-Forwarded-For`, a przy jego braku kasuje XFF — .NET liczy wtedy wiadro limitera po wewnętrznym adresie połączenia (osobnym, niewspólnym z biurem). Potwierdź adres w logu `user=api-key:dit_…` po pierwszym przebiegu.
- Service binding to nowe wejście do Workera DeliverIT: każdy Worker **na tym koncie**, który zadeklaruje binding, może podać dowolny `CF-Connecting-IP` limiterowi. Granica zaufania = konto Cloudflare.
- Rozwiązywanie nazwy `deliverit.internal` w kontenerze nie jest opisane w dokumentacji (przykłady pokazują tylko `http://my.worker` / `curl http://my.kv/…`). Objaw problemu: `dial tcp: lookup deliverit.internal … no such host` w `deliverit.errors` (run `207`, etap Productive bez zmian). Sprawdzenie przed pierwszym prawdziwym runem: `make dev` z kluczem o poprawnym kształcie w `.dev.vars` (`DELIVERIT_API_KEY=dit_` + 43 znaki `A-Z a-z 0-9 - _`, może być zmyślony — pod `wrangler dev` binding `DELIVERIT` działa lokalnie, bez `"remote": true`, więc produkcji DeliverIT nie woła), potem `curl -s -H "Authorization: Bearer $SYNC_KEY" 'http://localhost:8787/sync?dry_run=true' | jq .deliverit.errors`: po ~40 s ponowień `503 … Worker "deliverit-internal" not found` (albo `502 DELIVERIT_BINDING_FAILED`) = kontener doszedł do handlera (DNS i przechwycenie działają); `lookup deliverit.internal` = wirtualny host się nie rozwiązuje.
- Nazwa Workera `deliverit-internal` jest kontraktem bindingu: zmiana nazwy lub usunięcie zrywa etap (awaria bindingu → `502` → błąd w `deliverit.errors`).
- Cron `0 */4 * * *` (UTC) budzi kontener DeliverIT (`sleepAfter` 15 min) i bazę Neon **także w nocy** — mały koszt, sprzeczny z założeniem „w godzinach pracy” z runbooka DeliverIT.
- Kontrakt `/api/integrations/*` jest zamrożony **po obu stronach** tymi samymi literałami JSON (`container_src/deliverit_test.go` tutaj, testy .NET w `deliverit-internal`): zmiana reguły, limitu, kształtu JSON albo kodu błędu = zmiana obu repozytoriów.

## Lokalny dev

```bash
brew install go                       # opcjonalne; bez tego wszystko leci w Dockerze
cp .dev.vars.example .dev.vars        # uzupełnij sekrety
npm install

make test        # gofmt + go vet + go test (142 testy, < 1 s)
make build       # obraz linux/amd64, wypisuje architekturę
make run         # kontener na :8080 z .dev.vars
make dry-run     # curl 'localhost:8080/sync?dry_run=true&explain=true' | jq
make dev         # wrangler dev — pełna ścieżka Worker → DO → kontener
make typecheck   # wrangler types + tsc
make deploy      # test + wrangler deploy
make tail        # logi Workera i kontenera na żywo
```

W `wrangler dev` klawisz `[r]` przebudowuje kontener; kod Workera reloaduje się sam, kod kontenera nie.

**Cross-build arm64 → amd64.** Builder leci natywnie (`--platform=$BUILDPLATFORM`), Go cross-kompiluje przez `GOOS/GOARCH` z `CGO_ENABLED=0` — żaden kod amd64 nie wykonuje się w trakcie builda, więc QEMU/Rosetta nie wchodzi w grę. Finalny stage **musi** być prawdziwym obrazem bazowym przypiętym do amd64: BuildKit ignoruje `--platform` na `FROM scratch` i stempluje manifest architekturą hosta. Sprawdzone empirycznie — wersja na `scratch` dawała `linux/arm64`, co Cloudflare wymaga inaczej. `make build` wypisuje architekturę, żeby to nie przeszło niezauważone.

## Deploy

```bash
npx wrangler secret put SYNC_KEY CLICKUP_TOKEN PRODUCTIVE_TOKEN PRODUCTIVE_ORG_ID   # po kolei
npx wrangler secret put DELIVERIT_API_KEY   # opcjonalnie; Worker deliverit-internal musi już istnieć (binding)
npx wrangler deploy
# ODCZEKAJ kilka minut: kontenery provisionują się asynchronicznie i wcześniejsze
# wywołania będą błądzić, mimo że Worker już odpowiada
npx wrangler containers list
npx wrangler tail --format pretty
```

Pierwsze wywołanie: `?dry_run=true`. Oczekiwane `created: 0, updated: 0`, a w `deliverit` — `enabled: true`, `errors` puste (szczegóły: [Pierwsze wdrożenie](#pierwsze-wdrożenie-kroki-człowieka)).

## Weryfikacja przed cutoverem

**Oracle akceptacyjny:** dopóki .NET działa i właśnie skończył tick, ten port musi raportować **`created: 0, updated: 0`**. Każda niezerowa liczba to albo prawdziwy dryf, albo błąd portu — a `reasons` mówi natychmiast który.

Spodziewane, ograniczone odstępstwa (przewidź je **przed** spojrzeniem): sortowanie tagów daje jednorazowy PATCH per task z >1 tagiem, a trzy naprawy churnu mogą ujawnić, że .NET przepisywał setki tasków w każdym ticku.

1. `make test` — cały reconcile, mapowanie i polityka retry offline.
2. `make run` + `make dry-run` z prawdziwymi tokenami. Read-only jest wymuszone w `apiClient`, więc każdy zapis to twardy błąd.
3. Weź jedną akcję PATCH z `explain`, `curl`nij aktualne `custom_fields` tego taska i potwierdź, że planowane body jest nadzbiorem ze zmienionymi wyłącznie `242457`/`242565`.
4. **Bezpieczny test zapisu:** nowa task lista w projekcie `860646` (dziedziczy workflow, więc `161082`/`161083` zostają poprawne; custom fieldy są organizacyjne) + mała piaskownicowa lista ClickUp z rodzicem, dwoma subtaskami i „Szybką wyceną". Run z nadpisanymi `PRODUCTIVE_TASK_LIST_ID` i `CLICKUP_LIST_ID`. Sprawdź: create'y lądują, `parent_task` na subtaskach, estymaty, **drugi run = `created: 0, updated: 0`**, a po zmianie tytułu dokładnie jeden PATCH z `reasons: ["title"]`.
5. Test killowania: `docker kill --signal=TERM` w środku runu → graceful drain; `docker kill -9` → następny run kończy robotę **bez duplikatów**.
6. `make dev` i macierz auth (brak headera / zły klucz / klucz innej długości / klucz w query / `POST` / `HEAD` / nieznana ścieżka).
7. Deploy, kilka dni w `?dry_run=true` obok .NET, potem cutover: zatrzymaj .NET, zdejmij `dry_run`. Kontener .NET-a zostaw zatrzymany na tydzień.

## Różnice względem .NET

### 1:1
Lista ClickUp / task list / projekt / custom fieldy / workflow statusy / statusy „done" po polsku / pola „Szybka wycena" i tabela etykiet (z lukami) / priorytet `time_estimate` nad drop-downami i **sumowanie** obu / rozwiązywanie drop-downa (number → `orderindex`, string → `options[].id`, w fallbacku jako `orderindex`) / forma body z `relationships` (nieudokumentowana, ale sprawdzona w produkcji) / brak `data.id` w PATCH / **brak `parent_task` w PATCH** / kolejność rodzice-przed-dziećmi z guardem 64 i płaskim tworzeniem sierot / dedup po `id` (pierwszy wygrywa) / `subtasks=true` / `include_closed` nieustawione / `Name.Trim()` / brak ścieżki usuwania / pętla ClickUp do pustej strony.

### Zmienione świadomie
| Co | Dlaczego |
|---|---|
| Rate limiter zamiast `Task.Delay(4000)` | Limit Productive jest **na organizację**, dzielony z przeglądarkami zespołu. .NET robił 0,25 zapisu/s przypadkowo grzecznie; 1,0 rps to 4× szybciej i wciąż duży zapas pod 2,22 rps. |
| Merge `custom_fields` przy PATCH | Productive traktuje `custom_fields` jako **jeden atrybut-hash** — PATCH z dwoma kluczami znaczy „ustaw cały hash na te dwa" i kasuje resztę. Integracja jest właścicielem tylko `242457` i `242565`; wszystko inne na tasku (ustawione ręcznie albo przez inną integrację) nie należy do tej synchronizacji. Merge idzie z już pobranego taska, zero dodatkowych requestów. `MERGE_CUSTOM_FIELDS=false` przywraca stare zachowanie. |
| `remaining_time`/`start_date`/`due_date` pomijane w body | .NET wysyła je jako jawne `null` w **każdym** POST i PATCH (tylko `InitialEstimate` ma `WhenWritingNull`), więc każdy jego PATCH czyści w Productive `start_date` i `due_date`. To najlepszy dowód, że kasowanie pól przy PATCH-u jest skutkiem ubocznym serializacji, a nie decyzją. |
| Abort na non-2xx przy czytaniu + asercja `total_count` | Patrz „Bezpieczniki". |
| Bezpieczniki, detekcja duplikatów, `reasons`, `ambiguous` | Widoczność i ograniczony promień rażenia. |
| Tagi sortowane | .NET joinuje `HashSet` w losowej kolejności; sortowanie daje deterministyczne body (i golden testy) za cenę jednego PATCH-a per task z >1 tagiem. |
| Nazwy env `P`/`POrgId`/`CU` → pełne | Jednoliterowe nazwy w środowisku kontenera to realna kolizja. |

### Dwa limity Productive wykryte na pierwszym prawdziwym runie

Obie te sytuacje .NET **ukrywał** — nie sprawdzał statusu zapisu, więc te zadania nigdy nie trafiły do Productive i nikt się o tym nie dowiedział.

**Tytuł ponad 140 znaków** → `422 Invalid Attribute is too long (maximum is 140 characters) (data/attributes/title)`. Tytuł jest obcinany do `PRODUCTIVE_TITLE_MAX` znaków z wielokropkiem na końcu. Obcięcie następuje **przy mapowaniu ClickUp → Task, a nie przy budowaniu body** — i to jest istotne: Productive przechowuje wersję obciętą, więc porównywanie pełnego tytułu z ClickUp z tym, co leży w Productive, dawałoby różnicę w każdym runie i PATCH-a bez końca. Liczenie jest **po runach, nie bajtach** — limit jest w znakach, a polskie tytuły są pełne znaków wielobajtowych.

**Zbyt głębokie subtaski** → `422 Invalid Attribute invalid level of subtasks (data/attributes/parent_task)`. Productive przyjmuje `PRODUCTIVE_MAX_SUBTASK_DEPTH` poziomów zagnieżdżenia (domyślnie 1 — wartość wywnioskowana z tego właśnie błędu, nie z dokumentacji). Zadanie głębsze jest tworzone jako **płaskie** z ostrzeżeniem `subtask_too_deep`, a jego własne dzieci zaczynają liczenie od nowa, więc zachowujemy tyle struktury, ile Productive dopuszcza. Gdyby limit był ustawiony za wysoko, `Execute` ponawia create bez rodzica — to jedyne miejsce, gdzie ponowienie zapisu jest bezpieczne, bo `422` dowodzi, że nic nie powstało.

### Trzy naprawy wiecznego churnu
Kandydaci na wyjaśnienie, dlaczego `sleep(4s)` per zapis był w ogóle znośny. **Żadna nie zmienia danych — tylko przestają lecieć bezcelowe PATCH-e.**

1. **Puste tagi.** Zapisujemy `""` do `242565`; .NET czyta to przez `"".Split(", ")`, co w .NET zwraca `[""]` — zbiór z jednym pustym stringiem, wobec pustego zbioru po stronie ClickUp. `SetEquals` → false, więc **każdy task bez tagów jest PATCHowany w każdym runie, na zawsze.**
2. **Wyczyszczona estymata.** Body pomija `initial_estimate` gdy nil, ale `Equals` je porównuje: ClickUp null + Productive 480 → różnica → PATCH pomija pole → Productive bez zmian → znowu różnica. Na zawsze. `ESTIMATE_CLEAR_MODE=ignore` (default) uznaje ten przypadek za równy; `=null` konwerguje, ale kasuje estymaty wpisane ręcznie.
3. **0 vs nil.** `ms/60000` obcina, więc 30-sekundowa estymata to 0 minut; `nil ≡ 0`.

### Czego nie ruszać
1. **`include_closed`** — jedna flaga = tysiące POST-ów historycznych zamkniętych zadań, wyczerpany budżet API, zaśmiecona lista. Nieodwracalne bez masowego usuwania.
2. **`relationships` → płaskie `*_id`** — nieudokumentowane-ale-działające bije udokumentowane-ale-nietestowane.
3. **`parent_task` w PATCH** — dodanie przeparentowałoby taski, które ktoś ręcznie przeniósł. Zamiast tego warning `reparent_needed` (i `ALLOW_REPARENT=true`, gdy naprawdę chcesz).
4. **Retry POST-a na 5xx** — patrz wyżej.
5. **Ścieżka usuwania** — nie ma jej i nie dodajemy; zadanie zdjęte z listy ClickUp zostaje w Productive.

Sprawdzanie statusu PATCH-a (dziś `PCUIntegratorService.cs:175` je ignoruje) jest **dodane**. Spodziewaj się, że ujawni stos istniejących 422 — będzie wyglądać, jakby port coś zepsuł, a tylko uwidoczni.

## Do zmierzenia na produkcji (nie dało się bez tokenów)

Dry-run wypisuje wszystko, co potrzebne:

- **Prawdziwe nazwy statusów `161082`/`161083`** (`productive.workflow_status_names`). Mapowanie po nazwie zostało zachowane zgodnie z .NET; jeśli `161083` nie nazywa się dosłownie `Closed`, cała gałąź Done PATCHuje w kółko i zobaczysz to w `reasons.status`.
- **`clickup.status_types`** — czy `wydane` / `gotowe do wydania` / `zawieszone` są typu *closed*. Jeśli tak, są dziś **niewidoczne** dla fetcha (`include_closed=false`) i gałąź `→ Done` nigdy nie odpaliła. `ClickUpTask.Status.Type` jest w .NET czytane i nigdy nieużywane.
- **`productive.extra_custom_field_ids`** — czy merge jest w ogóle no-opem na tych danych. Jeśli lista jest pusta, temat kasowania custom fieldów zamyka się danymi.
- **`reasons` na pierwszym dry-runie** — ile z churnu z sekcji wyżej faktycznie leci; to kalibruje `PRODUCTIVE_RPS`, `MAX_WRITES` i realny czas runu.

## Runbook

- **Coś padło w połowie?** Odpal ponownie. Run jest idempotentny: klucz `242457` powstaje atomowo z taskiem, plan wynika wyłącznie z żywego stanu obu API, nie ma stanu lokalnego do zdezaktualizowania i nie ma ścieżki usuwania — najgorszy wynik to no-op.
- **`409`** — inny run trwa; `run_id` i `started_at` są w odpowiedzi. Lease wygasa 11 minut po starcie.
- **`422` + `aborted: "fetch_productive"`** — token, uprawnienia albo Productive ma awarię. Nic nie zostało zapisane.
- **`422` + `aborted: "max_creates_exceeded"`** — przeczytaj `actions` w odpowiedzi **przed** podniesieniem limitu. Bezpiecznik prawie zawsze ma rację.
- **`aborted: "rate_limited"`** — organizacja wyczerpała budżet Productive; run przerwany świadomie, zamiast backoffować w ciemno (Productive nie dokumentuje żadnych headerów rate-limitowych). Następny run dokończy.
- **`503` z Workera** — kontener się nie wystartował: `max_instances`, albo provisioning po pierwszym deployu jeszcze trwa.
- **`207` + `deliverit.errors`** — etap DeliverIT; etap Productive jest w porządku. `API_KEY_INVALID`/`UNAUTHENTICATED` → [rotacja klucza](#rotacja-klucza); `PROJECT_TASK_LIST_MISMATCH` → powiązanie zmieniło się w trakcie runu, następny run je podejmie; `DELIVERIT_BINDING_FAILED` → Worker `deliverit-internal` nie istnieje / nie odpowiada; `lookup deliverit.internal` → patrz „Znane ryzyka”; `without problem+json … Cloudflare challenged` → ruch poszedł przez strefę (tryb `direct`?).
- **`deliverit.enabled: false`** — `disabled_reason`: brak `DELIVERIT_BASE_URL` (celowo wyłączony) albo brak/zły sekret `DELIVERIT_API_KEY`.
- **exit code 137 w `onStop`** — OOM. Podnieś `instance_type`.
- **Logi:** `make tail`, albo dashboard → Workers & Pages → Containers → Logs. Retencja 7 dni na planie Paid; `observability.enabled` musi być `true` (jest).
- **Debug w kontenerze:** `npx wrangler containers ssh <instance>` — obraz jest na alpine, więc shell działa.
