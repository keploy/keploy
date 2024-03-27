package com.example.order.controllers;


import com.example.order.models.Order;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.http.ResponseEntity;
import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.PostMapping;
import org.springframework.web.bind.annotation.RequestBody;
import org.springframework.web.bind.annotation.RequestMapping;
import org.springframework.web.bind.annotation.RestController;
import com.example.order.services.*;
@RestController
@RequestMapping("/orders")
public class OrderController {

 

    @GetMapping("/")
    public ResponseEntity<Order> getOrders() {
        Order order1=new Order(Long.parseLong("10"),Long.parseLong("20"),5,15);
       
        return ResponseEntity.ok(order1);
    }

}