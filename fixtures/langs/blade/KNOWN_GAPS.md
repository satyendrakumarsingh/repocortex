# Blade known gaps

## A section named by a variable

```
@section($dynamic_name)
    <p>{{ $slot }}</p>
@endsection
```

The name is known only at run time, so it is not extracted: the query takes a
quoted literal only. The fixture ends with this block to hold that rule.

## Not definitions (decided, not gaps)

`@component`, `@include`, `@extends`, `@yield`, `@push`, `@slot`, echo
expressions, and a `@section` inside a `{{-- comment --}}`.
