# Abantu

Reverse proxy Minecraft clusterisable multi-domaines avec load balancing, synchro temps réel et persistance automatique des configurations distribuées.

## Caractéristiques principales
Core proxy / routage
- Routage par nom de domaine (analyse handshake Minecraft VarInt) avec fallback wildcard `*`.
- Multi-backends par domaine, stratégies: `round_robin`, `random`, `weighted_random`, `least_conn` (vrai comptage de connexions actives).
- Stratégie `weighted_random` via champ `weight`.
- Fallback automatique si le backend choisi échoue (dial avec exploration des autres).

Contrôles d’accès et limitation
- Whitelist / blacklist IP par host.
- Rate limiting par (domaine + IP) via Redis (fenêtre glissante simple + burst).
- Limitation handshake en mémoire (anti-scan) configurable (`handshake_security`).

Minecraft Status (MOTD)
- MOTD réel du backend (status ping natif) sans ré-écriture, avec cache par domaine (`ping_cache_ttl`).
- `unknown_host_motd` (domaine inconnu) & `backend_down_motd` (aucun backend disponible ou échecs).

Cluster & cohérence
- Gossip UDP avec horloge logique Lamport + version globale.
- Diff partiel (envoi uniquement des hosts modifiés) pour réduire la bande passante.
- Signature HMAC optionnelle des paquets gossip (`cluster.secret`) pour rejet des paquets forgés.
- Propagation des suppressions (tombstones simplifiés via flag `Deleted`).
- Persistance automatique locale (les hosts appris sont écrits dans `config.json`).

Sécurité & transport
- Auth API par tokens Bearer avec rôles `read` / `admin` (RBAC simple).
- Champs de configuration prévus pour mTLS (CA configurable) – enforcement mTLS à venir.
- Limitation handshake (token bucket local) métriquée.
- Support PROXY Protocol v1 (optionnel) pour conserver l’IP origine derrière un LB.
- TLS entrant côté proxy (certificat local) optionnel.

Observabilité
- Metrics Prometheus: connexions totales/actives, sélections backend (avec stratégie), échecs dial, état backend (gauge up/down), latence dial (histogramme), durée connexion (histogramme), connexions rate-limit, handshakes bloqués, hits/miss cache status, paquets gossip reçus et signatures invalides.
- Logs structurés (slog) JSON ou texte, rotation simple par redémarrage, flush fsync optionnel.

Ergonomie & résilience
- Génération automatique d’un `config.json` par défaut si absent.
- Backoff exponentiel sur backends en échec, health checks périodiques réactivant ceux qui reviennent.
- Fallback backend_down MOTD si tous échouent.

Roadmap (extraits)
- mTLS effectif API + rotation secrets à chaud.
- Protection anti-replay gossip (nonce / fenêtre temps).
- Compression/packaging multi-messages gossip.
- Garbage collection des tombstones.
- Histogramme latence ping backend / time-to-first-byte.

## Architecture rapide
```
Client -> (TCP) Abantu Proxy -> (TCP) Backend Minecraft
                 |            
                 |-- Redis (rate limit)
                 |-- API HTTP (gestion dynamique)
                 '-- UDP Gossip (synchro hosts inter-nœuds)
```

### Flux de synchronisation (Lamport + diff)
1. Modification via API -> création `HostInfo` (version timestamp + Lamport).
2. Horloge logique locale bumpée, insertion en mémoire, persistance.
3. Gossip envoie seulement les hosts dont la version est >= au dernier snapshot connu côté nœud émetteur.
4. Réception: validation HMAC (si secret), fusion si (version, lamport) plus récent, bump horloge locale, persistance.
5. Redémarrage: récupération depuis disque (pas de nœud maître requis).

## Format de configuration (exemple complet – avec sécurité)
```json
{
  "listen_host": "0.0.0.0",
  "listen_port": 25565,
  "redis_addr": "127.0.0.1:6379",
  "redis_password": "",
  "unknown_host_motd": "§cAucun backend n'est configuré pour ce domaine: {domain}",
  "backend_down_motd": "§cBackend indisponible pour {domain}",
  "server_name": "Abantu Proxy",
  "protocol_version": 760,
  "cluster": {
    "node_id": "node-a",
    "bind_addr": "1.1.1.1:14000",
    "peers": ["2.2.2.2:14000"],
    "gossip_interval": "3s",
    "seeds": ["1.1.1.1:14000"],
    "discovery": true,
    "secret": "change-me-shared-hmac"
  },
  "api_auth": {
    "tokens": {"my-admin-token": "admin", "readonly-token": "read"},
    "mtls": {"enabled": false, "ca_file": "ca.pem"}
  },
  "handshake_security": {"per_second": 30, "burst": 60},
  "proxy_protocol_enabled": false,
  "proxy_tls": {"enabled": false, "cert_file": "server.crt", "key_file": "server.key"},
  "hosts": [
    {
      "domain": "play.exemple.com",
      "strategy": "round_robin",
      "backends": [
        {"host": "10.0.0.10", "port": 25565, "weight": 1},
        {"host": "10.0.0.11", "port": 25565, "weight": 1}
      ],
      "backend_down_motd": "§cBackend principal HS pour {domain}, merci de réessayer.",
      "whitelist": [],
      "blacklist": ["203.0.113.10"],
      "rate_limit": {"connections_per_minute": 60, "burst": 20}
    },
    {
      "domain": "creative.exemple.com",
      "strategy": "weighted_random",
      "backends": [
        {"host": "10.0.1.10", "port": 25565, "weight": 3},
        {"host": "10.0.1.11", "port": 25565, "weight": 1}
      ],
      "whitelist": [],
      "blacklist": [],
      "rate_limit": {"connections_per_minute": 40, "burst": 10}
    },
    {
      "domain": "*",
      "strategy": "random",
      "backends": [
        {"host": "10.0.2.10", "port": 25565},
        {"host": "10.0.2.11", "port": 25565}
      ],
      "backend_down_motd": "§cAucun backend disponible actuellement pour {domain}.",
      "rate_limit": {"connections_per_minute": 30, "burst": 10}
    }
  ]
}
```

## Déploiement cluster (exemple 2 nœuds)
Machine A (1.1.1.1) et Machine B (2.2.2.2), même port gossip UDP 14000 ouvert.

Config A: peers = ["2.2.2.2:14000"], bind_addr = "1.1.1.1:14000"
Config B: peers = ["1.1.1.1:14000"], bind_addr = "2.2.2.2:14000"

Lancez sur chaque:
```bash
go run ./cmd/abantu -config config-a.json -api :8080
go run ./cmd/abantu -config config-b.json -api :8080
```

Ajoutez ensuite un host via l’API d’un nœud: il apparaît et est sauvegardé automatiquement sur l’autre.

## API HTTP (avec RBAC)
Base: `http://<ip_api>:<port>` (ex: :8080)

Endpoints:
- `GET /health` -> {status:"ok"}
- `GET /cluster/state` -> (rôle read/admin) dump JSON des HostInfo.
- `GET /hosts` -> (admin ou read si lecture seule? implémentation actuelle: admin pour write endpoints) liste hosts.
- `POST /hosts` -> (admin) créer: 
  ```json
  {
    "domain":"survie.exemple.com",
    "strategy":"round_robin",
    "backends":[ {"host":"10.0.3.10","port":25565} ],
    "rate_limit": {"connections_per_minute":60, "burst":20}
  }
  ```
- `GET /hosts/{domain}` -> host unique.
- `PUT /hosts/{domain}` -> mise à jour même format.
- `DELETE /hosts/{domain}` -> (admin) suppression.

Authentification: header `Authorization: Bearer <token>`.
Rôles:
- admin: lecture + écriture.
- read: accès endpoints lecture (`/cluster/state`, `/time`, `/version`, éventuellement `/hosts` GET selon politique — adaptation possible).

Chaque écriture:
1. Met à jour la config locale.
2. Sauvegarde `config.json`.
3. Emet un gossip (Update ou Delete) avec version incrémentée.

## Rate limiting
Clé: `domain:ip`. Incrément + TTL 60s. Si compteur > `connections_per_minute + burst` => rejet. (Simple; peut être renforcé plus tard par un algorithme token bucket précis.)

## Stratégies de load balancing
- round_robin: index cyclique par domaine.
- random: uniform random.
- weighted_random: distribution selon `weight` (>=1). Poids <=0 traités comme 1.
- least_conn: sélectionne le backend avec le moins de connexions actives.

## Journalisation
Logs structurés (slog) en JSON ou texte. Par défaut, si aucun paramètre fourni lors du lancement du binaire :
1. Génération automatique de `config.json` si absent.
2. Création/rotation automatique du fichier `logs/abantu.log` (ancien renommé avec timestamp).

Flags disponibles (optionnels):
```
-log.format json|text (défaut: text)
-log.level debug|info|warn|error (défaut: info)
-log.file chemin/fichier.log (défaut implicite: logs/abantu.log)
-log.sync (flush disque immédiat après chaque ligne)
```
Contenu log: événements de démarrage, requêtes API (hosts), connexions, sélection backend, échecs dial, états backends, métriques internes.

## Sécurité
Déjà présent:
- Auth API (tokens static RBAC).
- Signature HMAC gossip.
- Limitation handshakes.
- Rate limiting Redis.
- Whitelist/blacklist par host.
- TLS entrant & PROXY protocol v1.

À venir / améliorations:
- mTLS effectif (vérification cert client) pour API ou proxy.
- Rotation secrets (SIGHUP reload).
- Protection replay gossip (nonce / fenêtre temps).
- Chiffrement potentiel (optionnel) des paquets gossip.
- Filtrage géographique / ASN (extension future).

## Tâches futures (roadmap courte)
1. mTLS complet (API & proxy).
2. Reload dynamique (certs / secret cluster) sans redémarrage.
3. Histogrammes supplémentaires: ping latency backend, handshake parsing time.
4. Compression + agrégation multi-host gossip, GC tombstones.
5. Auth plus fine (scopes par endpoint) + éventuellement JWT.
6. Observabilité additionnelle: métriques circuit breaker, tentatives fallback.

## Démarrage rapide
Premier lancement minimal (tout automatique):
```bash
go build -o abantu ./cmd/abantu
./abantu
```
Ce qui se produit si fichiers absents:
- `config.json` généré avec un exemple fonctionnel.
- `logs/abantu.log` créé (rotation si existant non vide).

Lancement personnalisé:
```bash
./abantu -config myconfig.json -api :8080 -log.format json -log.level debug -log.sync
```

## Licence
MIT (à définir)

---
Abantu est un projet expérimental; contributions et suggestions bienvenues.
