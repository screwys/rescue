# Rescue!

A boring one-click to start server to make it less painful to debug on your one device via your other device! I have felt the need for this as I keep breaking my system, usually when working on [nocblue](https://github.com/screwys/nocblue), which usually results in needing to boot via a live ISO and it is painful for me to browse the internet on a stock browser. I also like to experiment using different operating systems on bare-metal, but I feel lazy to write the full link for my install script at github, or in some cases, the operating system doesn't even have my wi-fi card (FreeBSD PTSD...).

Which brings us to this. If you want to give it a try, clone this repo or grab a binary from [Releases](https://github.com/screwys/rescue/releases) page.

You can start the server with `./rescue`, or by opening the downloaded binary, and it opens the page on your browser. It starts the server at port 5000 by default, you can change this to whatever port is accessible just by editing `PORT=5000`, or running `PORT=5001 ./rescue`, it asks you to kill the process if port is busy. 

You can use the Web UI on both devices, or you can simply edit `rescue.toml` and save it. On Web UI, there are two simple side-by-side code blocks by default under `Instructions` and 1 block under ` Script`. You can write the instruction here and save, it turns into a code snippet. If you want to do it by editing the file, edit the instruction's `content` in `rescue.toml`, which is useful if you want to leverage a coding agent. You can resize the blocks and easily add another one from the site, or by adding another `[[instructions]]` entry. The another device can use the block on the right, or write some debugging log into another instruction block and save. The changes are loaded live on the site.

This is the same for scripts, they are in second section, after saving it will print the output the run the script, something like:

```sh
curl -fsSL http://your-ipv4-address:5000/install.sh | bash
```

[LICENSE](LICENSE)
