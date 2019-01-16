ngBigCache
=================

This is a library based on [bigcache](https://goreportcard.com/report/github.com/oastuff/clusteredBigCache) with some modifications to support
* clustering and
* individual item expiration

Bigcache is an excellent piece of software but the fact that items could only expire based on a predefined 
value was not just too appealing. Bigcache had to be modified to support individual expiration of items using
a single timer. This happens by you specifying a time value as you add items to the cache.
Running two or more instances of an application that would require some level of caching would normally
default to memcache or redis which are external applications adding to the mix of services required for your
application to run.

With clusteredBigCache there is no requirement to run an external application to provide caching for multiple
instances of your application. The library handles caching as well as clustering the caches between multiple
instances of your application providing you with simple library APIs (by just calling functions) to store and
get your values.

With clusteredBigCache, when you store a value in one instance of your application and every other instance 
or any other application for that matter that you configure to form/join your "cluster" will
see that exact same value.


##### credits
Core cache system from [bigcache](https://github.com/allegro/bigcache)

clusteredBigCache from [clusteredBigCache](https://github.com/nggenius/clusteredBigCache)

Data structures from [emirpasic](https://github.com/emirpasic/gods)

### LICENSE
MIT.




