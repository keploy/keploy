package com.example.product.controllers;


import com.example.product.models.*;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.http.ResponseEntity;
import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.RequestMapping;
import org.springframework.web.bind.annotation.RestController;
import org.springframework.web.client.RestTemplate;

import java.util.Arrays;
import java.util.List;

@RestController
@RequestMapping("/products")
public class ProductController {

    @Autowired
    private RestTemplate restTemplate;
    
    @GetMapping("/")
    public ResponseEntity<Order> getProduct() {
        ResponseEntity<Order> responseEntity = restTemplate.getForEntity(
                "http://localhost:8085/orders/",Order.class);
        return ResponseEntity.ok(responseEntity.getBody());
    }
    
}