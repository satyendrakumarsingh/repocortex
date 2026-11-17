{{-- Blade definitions. Rule:
     - @section('name') ... @endsection declares a function named name;
     - @section('name', 'inline value') declares the same name in one line;
     - @component, @include, @extends, @yield, @push, @foreach, echo expressions {{ }}, and @section text in a comment are not definitions.
--}}
@extends('layouts.app')

@section('title', 'Home')

@section('sidebar')
    <ul>@include('partials.nav')</ul>
@endsection

@component('components.alert', ['type' => 'info'])
    @slot('title')
        Notice
    @endslot
@endcomponent

@section('content')
    @foreach ($items as $item)
        {{ $item->name }}
    @endforeach
    @yield('footer')
@endsection

{{-- @section('commented') --}}
@push('scripts')
    <script src="app.js"></script>
@endpush

@section($dynamic_name)
    <p>{{ $slot }}</p>
@endsection
