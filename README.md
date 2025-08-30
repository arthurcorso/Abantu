# Abantu

Reverse proxy Minecraft clusterisable multi-domaines avec load balancing, synchro temps réel et persistance automatique des configurations distribuées.

## Caractéristiques principales
- Routage par nom de domaine (lecture protocole handshake Minecraft VarInt) avec fallback wildcard `*`.
- Multi-backends par domaine avec stratégies: `round_robin`, `random`, `weighted_random`, `least_conn` (placeholder pour l'instant).
- Stratégie `weighted_random` via champ `weight` sur chaque backend.
- Whitelist / blacklist IP par host.
- Rate limiting par (domaine + IP) via Redis (compteur glissant/minute + burst).
- MOTD personnalisables:
  - `unknown_host_motd` global (placeholders: `{domain}`)
  - `backend_down_motd` global + override par host.
- API REST pour gestion dynamique (sans redémarrage): création / mise à jour / suppression d’hôtes.
- Gossip UDP léger (cluster) pour diffuser les Hosts (multi-backends + stratégie + suppression) entre nœuds.
- Persistance automatique: chaque nœud écrit sur disque (dans son `config.json`) les hosts appris via cluster (ajouts & suppressions).
- Génération d’un fichier de configuration par défaut si absent.

## Architecture rapide
```
Client -> (TCP) Abantu Proxy -> (TCP) Backend Minecraft
                 |            
                 |-- Redis (rate limit)
                 |-- API HTTP (gestion dynamique)
                 '-- UDP Gossip (synchro hosts inter-nœuds)
```

### Flux de synchronisation
1. Un host est créé ou modifié via l’API sur un nœud A.
2. A met à jour sa config en mémoire, sauvegarde sur disque, envoie un `HostInfo` (avec version, backends JSON, stratégie) via gossip.
3. Les autres nœuds reçoivent, comparent la version et appliquent si plus récent (Upsert/Delete), puis sauvegardent à leur tour.
4. Un redémarrage ultérieur retrouve l’état complet sans dépendre d’un nœud “source”.

## Format de configuration (exemple complet)
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
    "gossip_interval": "3s"
  },
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

## API HTTP
Base: `http://<ip_api>:<port>` (ex: :8080)

Endpoints:
- `GET /health` -> {status:"ok"}
- `GET /cluster/state` -> dump JSON des HostInfo propagés.
- `GET /hosts` -> liste hosts courants (config en mémoire).
- `POST /hosts` -> créer: 
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
- `DELETE /hosts/{domain}` -> suppression (propagée + persistance).

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
- least_conn: placeholder -> actuellement random (TODO: implémenter compteur actif).

## Journalisation
Chaque connexion: domaine, protocole, backend choisi, erreurs de dial. Utile pour supervision initiale.

## Sécurité & Production (à prévoir)
- Ajouter authentification sur l’API (clé, JWT ou mTLS).
- Signatures ou chiffrement léger pour gossip (éviter injection externe UDP).
- Limiter l’exposition de l’API en interne seulement.
- Ajouter health checks backends + éviction automatique.

## Tâches futures (roadmap courte)
1. Implémentation réelle de `least_conn` (compteur atomic des connexions actives par backend).
2. Health checks périodiques (status + latence) et retrait temporaire.
3. Metrics Prometheus (connexions, échecs dial, distribution stratégies, rates limités).
4. Auth API + ACL.
5. Compression/dédup gossip, signature.
6. Support Proxy Protocol / TLS (optionnel) côté entrée.

## Démarrage rapide
```bash
go mod tidy
go run ./cmd/abantu -config config.json -api :8080
```

## Licence
MIT (à définir)

---
Abantu est un projet expérimental; contributions et suggestions bienvenues.
