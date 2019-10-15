#### <sub><sup><a name="4688" href="#4688">:link:</a></sup></sub> feature

* The pin menu on the pipeline page now matches the sidebar, and the dropdown toggles on clicking the pin icon #4688.

#### <sub><sup><a name="4556" href="#4556">:link:</a></sup></sub> feature

* Prometheus and NewRelic can receive Lidar check-finished event now #4556.

#### <sub><sup><a name="4707" href="#4707">:link:</a></sup></sub> feature

* Make Garden client HTTP timeout configurable. #4707

#### <sub><sup><a name="4698" href="#4698">:link:</a></sup></sub> feature

* @pivotal-bin-ju @taylorsilva @xtreme-sameer-vohra added batching to the NewRelic emitter and logging info for non 2xx responses from NewRelic #4698.

#### <sub><sup><a name="4470" href="#4470">:link:</a></sup></sub> feature, breaking

* All API payloads are now gzipped. This should help save bandwidth and make the web UI load faster. #4470

#### <sub><sup><a name="4448-4588" href="#4448-4588">:link:</a></sup></sub> feature

* You can now pin a resource to different version without unpinning it first #4448, #4588.

#### <sub><sup><a name="4507" href="#4507">:link:</a></sup></sub> fix

* @iamjarvo fixed a [bug](https://github.com/concourse/concourse/issues/4472) where `fly builds` would show the wrong duration for cancelled builds #4507.

#### <sub><sup><a name="4603" href="#4603">:link:</a></sup></sub> feature

* Job builds can now be [re-run](https://concourse-ci.org/managing-jobs.html#fly-rerun-build) with their old inputs #4603.
