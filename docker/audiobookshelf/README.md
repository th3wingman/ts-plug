```sh
$ docker buildx build --platform linux/amd64,linux/arm64 -t tailscale/ts-plug:audiobookshelf-2.30.0 -f docker/audiobookshelf/Dockerfile --load .

# running it
# no volumes currently, just for dev to make sure it works
$ docker run -it --rm --name "tsplug-audiobookshelf" -v tsplug-dev:/var/run/tsnet tailscale/ts-plug:audiobookshelf-2.30.0
```
