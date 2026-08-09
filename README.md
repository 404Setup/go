# The Go Programming Language

Go is an open source programming language that makes it easy to build simple,
reliable, and efficient software.

![Gopher image](https://golang.org/doc/gopher/fiveyears.jpg)
*Gopher image by [Renee French][rf], licensed under [Creative Commons 4.0 Attribution license][cc4-by].*

Our canonical Git repository is located at https://go.googlesource.com/go.
There is a mirror of the repository at https://github.com/golang/go.

Unless otherwise noted, the Go source files are distributed under the
BSD-style license found in the LICENSE file.

### About this fork
This fork is a personal Go branch maintained by 404Setup, and it has the following improvements:

- std: sync/v2 - It implements generic versions of `sync.Pool` and `sync.Map`, as well as an atomic ordered map.
- std: sync/atomic/v2 - Implemented a generic version of `atomic.Value`
- std: container/v2 - Implemented ordered containers
- gc: Slightly optimized some parts of sync.Pool (I'm not sure if it works because its commits are mixed up with other commits, so I can't test it)
- compiler: To resolve a passive, malicious memory leak bug from Kaspersky, GOFIPS140 was actually removed from the binary when it was disabled. Previously, it would always be compiled instead of being eliminated by the dead code eliminater.

None of the commits will be merged upstream because I cannot guarantee the quality of my code and I do not have time 
to wait for the pr queue.

If you wish to use my code, please comply with the license.

Want to download it? You can get it from the releases; our versioning rules are consistent with golang/go.

Want to set it up in actions? We'll be releasing an action soon so you can seamlessly migrate to our fork. Until then, 
you can check out the examples in my project, which are licensed under the MPL-2.0 license. Once released as a release, 
it will be licensed under the Apache-2.0 license.

Before reporting an issue, you should confirm whether the problem originated with us or our upstream. You should not report 
bugs caused by us to our upstream.

We use streaming version updates, meaning we merge whatever our upstream updates in the master branch. 
We do not maintain LTS versions.

### Download and Install

#### Binary Distributions

Official binary distributions are available at https://go.dev/dl/.

After downloading a binary release, visit https://go.dev/doc/install
for installation instructions.

#### Install From Source

If a binary distribution is not available for your combination of
operating system and architecture, visit
https://go.dev/doc/install/source
for source installation instructions.

### Contributing

Go is the work of thousands of contributors. We appreciate your help!

To contribute, please read the contribution guidelines at https://go.dev/doc/contribute.

Note that the Go project uses the issue tracker for bug reports and
proposals only. See https://go.dev/wiki/Questions for a list of
places to ask questions about the Go language.

[rf]: https://reneefrench.blogspot.com/
[cc4-by]: https://creativecommons.org/licenses/by/4.0/
