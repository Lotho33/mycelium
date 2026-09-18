# Nodo appliance (Wyse 3040/5070) — installazione Debian + setup mycelium

Runbook operativo per preparare un mini-PC (Dell Wyse 3040/5070, eMMC
saldata) come nodo mycelium da consegnare così com'è a chi non deve
configurare nulla: cavo ethernet + corrente, basta.

Stessa chiavetta USB riutilizzabile per tutte le macchine (ISO scritta con
`dd`, vedi sessione — `debian-13.7.0-amd64-netinst.iso`, boot da **"Partition
2"** in UEFI, installer in modalità testuale **"Install"**, non graphical).

Questo file viene aggiornato mano a mano che i passi vengono verificati dal
vivo — solo i passi confermati corretti restano qui. Diventerà la base per
uno script di post-setup automatico.

---

## Parte 1 — Installer Debian (verificato, flusso standard d-i)

Segui le schermate in quest'ordine, con queste risposte:

| Schermata | Scelta |
|---|---|
| Boot menu chiavetta | **Partition 2** (EFI), poi **Install** (non Graphical install) |
| Select a language | English *(consigliato per un server: log/doc coerenti; puoi mettere Italian se preferisci, non cambia nulla di sostanza)* |
| Select your location | Italy |
| Configure the keyboard | Italian *(o il layout fisico reale della tastiera collegata)* |
| Detect network hardware | automatico |
| Network configuration → Configure the network | **automatico via DHCP** — sì |
| Hostname | nome univoco per macchina, es. `mycelium-node1` (secondo nodo → `mycelium-node2`, ecc.) |
| Domain name | lascia vuoto |
| Root password | **lascia vuoto** → Debian crea l'utente normale con sudo abilitato e disabilita il login diretto di root (default più sicuro, meno da disfare dopo) |
| Full name for the new user | es. `mycelium admin` |
| Username | `mycelium` |
| Password utente | scegli una password robusta, te la ricordi: serve per SSH/sudo su ogni nodo |
| Configure the clock | Europe/Rome (o conferma auto-rilevato) |
| **Partition disks → metodo** | **Guided - use entire disk** *(niente LVM, niente encryption — non serve per questo uso)* |
| **Partition disks → disco** | **attenzione**: su eMMC il disco si chiama `/dev/mmcblk0`, NON `/dev/sda`. Su una macchina con un solo disco è comunque l'unica voce disponibile. |
| Partitioning scheme | **All files in one partition** |
| Finish partitioning and write changes | Yes |
| Scan another CD or DVD? | No |
| Use a network mirror? | **Yes** |
| Mirror country | Italy (o la voce generica `deb.debian.org` in cima alla lista se presente — va bene uguale, si geolocalizza da sola) |
| HTTP proxy | vuoto |
| Participate in package survey | No |
| **Software selection (tasksel)** | deseleziona tutto tranne: **SSH server** + **standard system utilities**. Nessun desktop environment, nessun web/print server. |
| Install the GRUB boot loader | Yes → installa sul disco visto sopra (`/dev/mmcblk0` su eMMC) |
| Installation complete | Continue → rimuovi la chiavetta USB quando richiesto, riavvia |

Al primo boot dopo il riavvio: login da console (o SSH via `ssh <utente>@<ip-dhcp>`,
lo trovi con un giro sul router di casa in questa fase iniziale — dopo
mettiamo avahi per non doverlo più cercare).

> ⚠️ **Importante, verificato sul nodo 1 (Wyse 5070)**: se il cavo ethernet
> NON è collegato durante l'installer, Debian rileva solo il WiFi e ti
> chiede SSID+password della tua rete — che finiscono salvate in chiaro
> (leggibili da root) in `/etc/network/interfaces` sul disco che poi parte
> per casa d'altri. **Collega il cavo ethernet PRIMA di avviare
> l'installer**, così questa schermata non compare nemmeno e non c'è nulla
> da ripulire dopo. Se ti è già successo, vedi il primo passo della Parte 2
> per rimuoverle.

---

## Parte 2 — Setup post-installazione per mycelium

*Passi verificati dal vivo sul nodo 1 (Wyse 5070, hostname `forlo`, IP
10.0.0.192 sulla LAN di casa in questa fase di test).*

### Accesso amministrativo
Se durante l'installer hai messo **una password di root** (non l'hai
lasciata vuota): `sudo` **non viene installato automaticamente** e
l'utente creato non ha privilegi — verificato. Per operare serve una
chiave SSH su `root` direttamente:
```
ssh-keygen -t ed25519 -f ~/.ssh/id_ed25519 -N ""   # una tantum, sul tuo PC
```
poi, **dalla console fisica del nodo** (non funziona da remoto: root
accetta solo chiavi, non password, via SSH — comportamento di default
Debian, lasciato così):
```
mkdir -p /root/.ssh && chmod 700 /root/.ssh
echo '<la-tua-chiave-pubblica>' >> /root/.ssh/authorized_keys
chmod 600 /root/.ssh/authorized_keys
```
Da lì in poi: `ssh root@<ip-nodo>` funziona senza password, chiave sola.

### Rete: solo ethernet, WiFi rimosso (se presente)
Se l'installer ha configurato il WiFi (vedi avviso sopra), su `root@<ip>`:
```bash
cat > /etc/network/interfaces << 'EOF'
# This file describes the network interfaces available on your system
# and how to activate them. For more information, see interfaces(5).

source /etc/network/interfaces.d/*

# The loopback network interface
auto lo
iface lo inet loopback

# Ethernet — unica interfaccia di rete usata da questa appliance
auto enp1s0
iface enp1s0 inet dhcp
EOF
ifdown wlp0s12f0 2>&1
systemctl disable --now wpa_supplicant.service
ip link set wlp0s12f0 down
```
(sostituisci `enp1s0`/`wlp0s12f0` con i nomi reali delle interfacce di quel
nodo — verificali con `ip link show` prima, cambiano da macchina a
macchina).

**Verificato con riavvio reale**: dopo `reboot`, il nodo risale da solo
con IP DHCP solo su ethernet, WiFi giù, nessuna credenziale rimasta su
disco (`grep -i 'psk\|ssid' /etc/network/interfaces` non trova nulla).

**Nota per la prossima volta**: se il cavo è collegato fin dall'inizio
(vedi avviso Parte 1), questo intero passo di pulizia non serve — l'unica
interfaccia configurata dall'installer sarà già quella ethernet.

### Avahi (discovery `.local`)
```bash
apt-get update -qq
apt-get install -y -qq avahi-daemon
systemctl enable --now avahi-daemon
```
**Verificato**: da un'altra macchina della stessa LAN, `<hostname>.local`
si risolve da solo (testato con `getent hosts`/`ping`) — zero config
aggiuntiva, avahi pubblica l'hostname di sistema di default.

### Docker Engine (repo ufficiale, non i pacchetti Debian)
```bash
apt-get install -y -qq ca-certificates curl gnupg
install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc
chmod a+r /etc/apt/keyrings/docker.asc

. /etc/os-release
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian $VERSION_CODENAME stable" \
  > /etc/apt/sources.list.d/docker.list

apt-get update -qq
apt-get install -y -qq docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
systemctl enable --now docker
```
**Verificato**: il repo ufficiale Docker supporta già `trixie` (Debian 13)
— niente bisogno di fallback su `bookworm`. `docker compose` è il
sub-comando (plugin), non il vecchio binario `docker-compose` a parte.

### Deploy dello stack mycelium
Dal tuo PC (repo mycelium-core), copia i due file necessari sul nodo:
```bash
ssh root@<ip-nodo> "mkdir -p /opt/mycelium/data"
scp docker-compose.prod.yml cobweb.toml root@<ip-nodo>:/opt/mycelium/
```
Poi sul nodo:
```bash
cd /opt/mycelium
docker compose -f docker-compose.prod.yml up -d --remove-orphans
docker compose -f docker-compose.prod.yml ps   # attesi 5 container "healthy"
```
Nessun `docker login` necessario: le immagini (`ghcr.io/lotho33/mycelium`,
`ghcr.io/lotho33/cobweb`) sono pubbliche.

**Bug reale trovato e corretto alla fonte (già fixato in
`docker-compose.prod.yml`, non serve più rifarlo sui prossimi nodi)**: le
immagini recenti di `microwarp:latest` rifiutano di avviarsi come proxy
SOCKS5 senza autenticazione esplicita, a meno di impostare
`ALLOW_NO_AUTH=1` — cambiamento arrivato dopo l'ultima verifica di questo
compose. Sicuro da impostare qui: la porta è pubblicata solo su
`127.0.0.1`, mai sulla LAN.

**Verificato — stato finale sul nodo 1**: tutti e 5 i container
(`mycelium`, `cobweb`, `redis`, `microwarp`, `watchtower`) `healthy`,
`curl http://localhost:8000/health` → `{"status":"ok"}`, avvio automatico
garantito dai `restart: unless-stopped` dei container + `docker.service`
abilitato al boot (nessuna unit systemd aggiuntiva necessaria).

### Problema noto, non ancora risolto (cosmetico)
`/root/.docker/config.json` finisce creato come **directory** invece che
file al primo `docker compose up` (bug noto di Docker: se il file bind-
montato da watchtower non esiste, Docker crea una dir al suo posto).
Genera un warning innocuo nei log di ogni comando `docker compose` — non
blocca nulla (le immagini sono pubbliche, Watchtower le legge comunque).
Fix noto ma non ancora applicato: `rmdir` quella directory e sostituirla
con un file `{}`.

### Hostname avahi: `mycelium`, non il nome dell'amico
Deciso in sessione: ogni nodo finisce su una rete diversa (casa di un
amico diverso), quindi non c'è mai conflitto tra più nodi con lo stesso
nome mDNS — usare sempre `mycelium` come hostname è più comodo per te
(un solo nome da ricordare per tutti i nodi) di un nome diverso per amico.
```bash
hostnamectl set-hostname mycelium
systemctl restart avahi-daemon
```
**Verificato**: `mycelium.local` si risolve correttamente da un'altra
macchina della LAN subito dopo il cambio, nessun riavvio necessario.

### Log: rotazione, altrimenti crescono senza limite
Tre punti distinti, tutti verificati necessari su questo setup:

1. **Docker non limita i log dei container per default** — causa più
   comune di "il disco si riempie da solo dopo mesi" su un'appliance non
   presidiata. Fix a livello daemon (si applica a tutti i container,
   presenti e futuri, dopo un force-recreate):
   ```bash
   cat > /etc/docker/daemon.json << 'EOF'
   {
     "log-driver": "json-file",
     "log-opts": { "max-size": "10m", "max-file": "3" }
   }
   EOF
   systemctl restart docker
   cd /opt/mycelium && docker compose -f docker-compose.prod.yml up -d --force-recreate
   ```
   Verificato con `docker inspect <container> --format '{{.HostConfig.LogConfig}}'`
   → `max-file:3 max-size:10m` applicato. Cap totale ~150MB per tutto lo
   stack (5 servizi × 30MB).

2. **journald senza cap esplicito**:
   ```bash
   # in /etc/systemd/journald.conf, dentro [Journal]:
   SystemMaxUse=100M
   MaxRetentionSec=2week
   systemctl restart systemd-journald
   ```

3. **mycelium scrive un file di log al giorno (`data/logs/<data>.log`) e
   non li elimina mai da solo** — verificato leggendo
   `cmd/server/dailylog.go`: ruota per data ma non c'è alcuna pulizia dei
   file vecchi, si accumulano all'infinito. Fix operativo (non tocca il
   codice Go, un timer systemd che pota i file più vecchi di 30 giorni):
   ```bash
   cat > /etc/systemd/system/mycelium-logs-cleanup.service << 'EOF'
   [Unit]
   Description=Elimina i log giornalieri di mycelium piu vecchi di 30 giorni
   [Service]
   Type=oneshot
   ExecStart=/usr/bin/find /opt/mycelium/data/logs -name "*.log" -mtime +30 -delete
   EOF
   cat > /etc/systemd/system/mycelium-logs-cleanup.timer << 'EOF'
   [Unit]
   Description=Esegue mycelium-logs-cleanup ogni giorno
   [Timer]
   OnCalendar=daily
   Persistent=true
   [Install]
   WantedBy=timers.target
   EOF
   systemctl daemon-reload
   systemctl enable --now mycelium-logs-cleanup.timer
   ```
   *Miglioria futura possibile lato codice*: far pota-re i file vecchi
   direttamente a `dailyLogFile` invece di delegarlo a un timer esterno —
   non fatto in questa sessione, solo annotato.

### zram al posto (in aggiunta) dello swap su eMMC
L'installer crea da solo una partizione swap sull'eMMC (790MB,
`/dev/mmcblk0p3`) — scritture di swap su flash saldata sono da evitare
quando possibile (usura, prestazioni scarse). Con la RAM reale disponibile
su questi nodi, lo swap vero servirà comunque di rado: zram lo copre senza
toccare il disco.
```bash
apt-get install -y zram-tools
# default in /etc/default/zramswap già adatti: ALGO=lz4 (CPU debole →
# preferire velocità a rapporto di compressione), PERCENT=50, PRIORITY=100
systemctl restart zramswap
```
**Verificato**: `swapon --show` → `zram0` priorità 100 vs `-2` della
partizione eMMC, quindi il kernel usa zram per primo e lascia l'eMMC come
ultima risorsa. Su nodo 1 (3.7GB RAM): 1.8GB di zram attivi.

### Controllo servizi abilitati (debloat)
`systemctl list-unit-files --state=enabled` verificato: lista già minimale
grazie alla scelta di soli "SSH server + standard system utilities"
nell'installer (niente MTA, NetworkManager, stampa, X11). Nessuna rimozione
necessaria — `fstrim.timer`/`e2scrub_*` anzi utili qui (TRIM periodico su
eMMC, controllo filesystem senza intervento manuale).

### Non ancora fatto
- Hardening finale rete/SSH prima della consegna reale (rivedere
  l'accesso root, valutare se serve altro).
- Replica di tutta la Parte 2 sul secondo nodo (Wyse 3040) per confermare
  che regge anche lì.
- `/root/.docker/config.json` come directory invece che file (vedi sopra,
  cosmetico, bloccato dal classificatore di sicurezza).
