## Quick notes

```sh
# build container
$ docker buildx build --platform linux/amd64,linux/arm64 -t tailscale/ts-plug:pihold-2025.10.03 -f docker/pi-hole/Dockerfile --load .

# run container
$ docker run -it --rm --name "tsplug-dns" -v tsplug-dns:/var/run/tsnet tailscale/ts-plug:pihold-2025.10.03
```
