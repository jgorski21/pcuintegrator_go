# pcuintegrator_go

Jednokierunkowa synchronizacja zadań **ClickUp → Productive.io**. Port `pcuIntegrator_dotnet` na Go, uruchamiany jako **Cloudflare Container** za **Workerem**, odpalany **`GET /sync` z kluczem w headerze** — bez wewnętrznego timera. Między runami wszystko śpi.

```
klient  ──GET /sync  Authorization: Bearer <SYNC_KEY>──▶  Worker (src/index.ts)
                                                            │ auth stałoczasowa, 401/405/404
                                                            ▼
                                                     SyncContainer (Durable Object)
                                                            │ lease lock → 409 gdy run trwa
                                                            ▼
                                                     kontener: Go HTTP :8080
                                                            │
                                                            ▼
                                              api.clickup.com  +  api.productive.io
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

Kody: `200` OK · `207` część zapisów padła · `422` run przerwany (`aborted`) · `409` inny run trwa · `401`/`405`/`404` · `503` kontener niedostępny.

## Sekrety

```bash
npx wrangler secret put SYNC_KEY            # openssl rand -hex 32
npx wrangler secret put CLICKUP_TOKEN       # surowy pk_… , bez "Bearer"
npx wrangler secret put PRODUCTIVE_TOKEN    # X-Auth-Token
npx wrangler secret put PRODUCTIVE_ORG_ID   # X-Organization-Id
# opcjonalnie na czas rotacji:
npx wrangler secret put SYNC_KEY_PREVIOUS
```

`SYNC_KEY` jest sprawdzany **tylko w Workerze** i nigdy nie trafia do kontenera — inaczej wylądowałby w logach kontenera i w `/proc/self/environ`. Nigdy nie używaj `image_vars`: to build args, wypalają się w warstwach obrazu.

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
| `SYNC_TIMEOUT` | `10m` | walidowane ≤ 12m |
| `MAX_RETRIES` | `4` | |
| `LOG_LEVEL` | `info` | JSON na stdout |

Brakujący `CLICKUP_TOKEN` / `PRODUCTIVE_TOKEN` / `PRODUCTIVE_ORG_ID` → proces **nie startuje**. Puste `PRODUCTIVE_TOKEN` prowadziłoby do 401 na każdym czytaniu, a to najgroźniejszy stan w całym programie (patrz „Dlaczego czytanie przerywa run").

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
  "warnings": [], "errors": []
}
```

**`reasons` to najważniejsze pole.** Zbiegnięty system robi 0 zapisów; `{"tags": 412}` w każdym runie znaczy, że coś nigdy nie konwerguje i każdy run przepisuje pół listy. `status_types` i `workflow_status_names` są tam po to, żeby dwie otwarte kwestie z sekcji „Do zmierzenia" rozstrzygnąć danymi, bez osobnego curla.

`ambiguous` to zapisy, których wynik jest nieznany (utracona odpowiedź POST-a). Nie są ponawiane — patrz niżej.

## Bezpieczniki i decyzje, które warto znać

**Dlaczego czytanie przerywa run.** Każdy non-2xx przy czytaniu Productive kończy run bez zapisu. .NET jest bezpieczny **przez przypadek**: `GetFromJsonAsync` rzuca na non-2xx i wyjątek leci do `catch`. Naiwny port (`if err != nil` + `Decode`) zdekodowałby stronę błędu jako pustą listę, uznał, że Productive jest puste, i **wysłał POST każdego zadania z ClickUp** — każde ze wpisanym `242457`, więc nie do odróżnienia od prawdziwych. Dodatkowo liczba zebranych zadań jest porównywana z `meta.total_count`: krótszy odczyt (task przesunął się między stronami w trakcie czytania) też przerywa run.

**Bezpieczniki `MAX_CREATES` / `MAX_WRITES`.** Przekroczenie = abort **przed** pierwszym zapisem, z planem w odpowiedzi. To software bez nadzoru za triggerem co 4h; bez tego regresja w mapowaniu przepisuje całą listę, zanim ktokolwiek zauważy.

**Retry tylko tam, gdzie bezpieczny.** GET: 429/5xx/transport. PATCH: 429/5xx (body w pełni opisuje stan docelowy). **POST: wyłącznie 429** — 429 dowodzi, że zapis się nie stał, natomiast timeout/5xx/reset są **niejednoznaczne** i retry stworzyłby bliźniaka. Utrata odpowiedzi POST-a jest samonaprawialna: klucz `242457` powstaje atomowo z taskiem, więc następny run go znajdzie i zrobi PATCH. **Nie „naprawiaj" tego dodając retry.**

**Duplikaty `242457`.** Jeśli dwa taski Productive zgłaszają ten sam ClickUp id, **nie zapisujemy do żadnego** i raportujemy oba id. .NET w tej sytuacji wywala się na `ToDictionary` przy każdym ticku (trwała awaria), a naiwne `map[k]=v` cicho wybrałoby jednego bliźniaka.

**Lease lock w Durable Object.** Mutex w procesie Go nie chroni przed dwiema generacjami kontenera: przy rollout/recyklingu hosta stary proces ma 15 minut na dokończenie, a DO rutuje nowe requesty do następcy — dwa runy czytałyby Productive przed pierwszym zapisem i oba wysłałyby POST tego samego taska. DO to jedyne miejsce ze stanem przeżywającym restart kontenera. Mutex w Go zostaje jako druga linia (i dla lokalnego `docker run`).

**Cykl życia kontenera.** `sleepAfter = "20m"` to wyłącznie backstop — deadline runu to 10 min, więc timer nie może wystrzelić w trakcie. Po czystym zakończeniu DO woła `stop()`, więc kontener ginie od razu po runie. Po zerwanym połączeniu `stop()` **nie** jest wołane, a lease wygasa sam: proces Go pracuje na kontekście odpiętym od requestu, więc zerwane połączenie kosztuje podsumowanie, nie run — wynik zostaje w `/status` i w logach.

## Lokalny dev

```bash
brew install go                       # opcjonalne; bez tego wszystko leci w Dockerze
cp .dev.vars.example .dev.vars        # uzupełnij sekrety
npm install

make test        # gofmt + go vet + go test (64 testy, ~0.03 s)
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
npx wrangler deploy
# ODCZEKAJ kilka minut: kontenery provisionują się asynchronicznie i wcześniejsze
# wywołania będą błądzić, mimo że Worker już odpowiada
npx wrangler containers list
npx wrangler tail --format pretty
```

Pierwsze wywołanie: `?dry_run=true`. Oczekiwane `created: 0, updated: 0`.

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
- **exit code 137 w `onStop`** — OOM. Podnieś `instance_type`.
- **Logi:** `make tail`, albo dashboard → Workers & Pages → Containers → Logs. Retencja 7 dni na planie Paid; `observability.enabled` musi być `true` (jest).
- **Debug w kontenerze:** `npx wrangler containers ssh <instance>` — obraz jest na alpine, więc shell działa.
